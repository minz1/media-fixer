package stuckread_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func TestRerunRefusedWhileRestartingLogsInfoAndRetries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	inc := openOne(t, h)
	h.setStatus(inc.ID, db.StatusAgentFixed)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.svc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	h.poll(three("T", 7)...)
	h.poll(three("T", 7)...)
	if got := h.status(inc.ID); got != db.StatusAgentFixed {
		t.Fatalf("status = %s, want agent_fixed while restarting", got)
	}
	if n := h.count("WARN"); n != 0 {
		t.Errorf("warn lines = %d, want none for a refusal during shutdown:\n%s", n, h.logs)
	}
	refused := 0
	for line := range strings.SplitSeq(h.logs.String(), "\n") {
		if strings.Contains(line, "level=INFO") && strings.Contains(line, "media-fixer is restarting") {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("INFO refusals = %d, want 2 (the refusal must not use up the once):\n%s", refused, h.logs)
	}
}
