package sweep_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/sweep"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) count(level string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for line := range strings.SplitSeq(b.buf.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, "pack sweep scheduler") {
			n++
		}
	}
	return n
}

func TestScheduler_CorruptSettingsWarnsOncePerDistinctError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.runner.Log = slog.New(slog.NewTextHandler(logs, nil))
	ctx := context.Background()
	sch := sweep.NewScheduler(ctx, h.runner, h.db)
	minute := 0
	tickN := func(n int) {
		for range n {
			minute++
			h.tick(sch, t0().Add(time.Duration(minute)*time.Minute))
		}
	}

	if err := h.db.SetSetting(ctx, sweep.SettingsKey, `{"enabled":true,"max_grabs":"x"}`); err != nil {
		t.Fatal(err)
	}
	tickN(3)
	if n := logs.count("WARN"); n != 1 {
		t.Errorf("WARNs after 3 ticks on one corrupt row = %d, want 1", n)
	}

	if err := h.db.SetSetting(ctx, sweep.SettingsKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	tickN(3)
	if n := logs.count("WARN"); n != 2 {
		t.Errorf("WARNs after the error changed = %d, want 2", n)
	}

	if err := sweep.SaveSettings(ctx, h.db, sweep.DefaultSettings(), h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	tickN(3)
	if n := logs.count("WARN"); n != 2 {
		t.Errorf("WARNs after the row was fixed = %d, want still 2", n)
	}
	if n := logs.count("INFO"); n != 1 {
		t.Errorf("INFOs after the row was fixed = %d, want 1", n)
	}
}
