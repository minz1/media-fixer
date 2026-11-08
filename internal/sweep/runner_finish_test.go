package sweep_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

const (
	blockFinishSQL = `CREATE TRIGGER block_finish BEFORE UPDATE ON sweep_runs BEGIN SELECT RAISE(FAIL, 'blocked'); END`
	dropBlockSQL   = `DROP TRIGGER block_finish`
)

func (b *lockedBuffer) levelCount(level string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), "level="+level)
}

func (b *lockedBuffer) has(level, msg string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for line := range strings.SplitSeq(b.buf.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, msg) {
			return true
		}
	}
	return false
}

func TestS1_9_OutcomeTable(t *testing.T) {
	t.Parallel()
	cancelled := context.Canceled
	tests := []struct {
		name      string
		err       error
		ctxErr    error
		want      sweepstate.Outcome
		wantReson string
	}{
		{"nil error stays ok after cancel", nil, cancelled, sweepstate.OutcomeOK, ""},
		{"nil error", nil, nil, sweepstate.OutcomeOK, ""},
		{"paused", sweep.ErrPaused, nil, sweepstate.OutcomePaused, ""},
		{
			"wrapped ctx error",
			fmt.Errorf("lookup: %w", context.Canceled), cancelled,
			sweepstate.OutcomeCancelled, cancelled.Error(),
		},
		{"error", errors.New("boom"), nil, sweepstate.OutcomeError, "boom"},
		{
			"redownload beats cancel",
			sweep.ErrRedownloadForTest(), cancelled,
			sweepstate.OutcomeRedownload, sweep.ErrRedownloadForTest().Error(),
		},
		{"empty error message", errors.New(""), nil, sweepstate.OutcomeError, "error"},
		{"empty cause message", errors.New("gone"), errors.New(""), sweepstate.OutcomeCancelled, "cancelled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := sweep.OutcomeForTest(tc.err, tc.ctxErr)
			if got != tc.want || reason != tc.wantReson {
				t.Errorf("outcome = %v %q, want %v %q", got, reason, tc.want, tc.wantReson)
			}
		})
	}
}

func fixtureHarness(t *testing.T) (*harness, *sweep.Scheduler) {
	t.Helper()
	h, sch := scheduledAt(t, t0(), settings())
	h.fake.addSeries(1, "Show", missingEps(1, 8, 81, 82, 83)...)
	h.fake.setReleases(seasonKey(1, 8), loadReleases(t, "rel-sv-8.json.gz")...)
	return h, sch
}

func TestS1_9_CompletedRunStaysDoneAfterCancel(t *testing.T) {
	t.Parallel()
	probe, psch := fixtureHarness(t)
	var reads atomic.Int32
	probeNow := probe.runner.Now
	probe.runner.Now = func() time.Time {
		reads.Add(1)
		return probeNow()
	}
	if err := psch.RunNow("manual", false); err != nil {
		t.Fatal(err)
	}
	psch.Wait()
	total, last := probe.fake.requestCount(), reads.Load()
	if total == 0 || last == 0 {
		t.Fatal("the probe run made no Sonarr request or clock read")
	}

	h, sch := fixtureHarness(t)
	var seen atomic.Int32
	now := h.runner.Now
	h.runner.Now = func() time.Time {
		if seen.Add(1) == last {
			sch.CancelLive(errors.New("cancelled after the last request"))
		}
		return now()
	}
	if err := sch.RunNow("manual", false); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != sweepstate.Done {
		t.Fatalf("runs = %+v, want one run done", runs)
	}
	if got := h.fake.requestCount(); got != total {
		t.Errorf("Sonarr requests = %d, want %d: nothing after the cancel", got, total)
	}
	n, _, err := h.db.RetryableSweepRunsSince(context.Background(), t0().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Errorf("retryable runs = %d, %v; want the slot not retried", n, err)
	}
}

func TestS1_11_FinishRetriedAfterWriteError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.startErr = card.ErrUntracked
	h.rawExec(t, blockFinishSQL)
	var sleeps []time.Duration
	sleep := h.runner.Sleep
	h.runner.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		if len(sleeps) == 1 {
			h.rawExec(t, dropBlockSQL)
		}
		return sleep(ctx, d)
	}
	run := h.run(t, false, settings())
	if run.Status != sweepstate.Done {
		t.Errorf("status = %s, want done", run.Status)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Errorf("sleeps = %v, want one of 1s", sleeps)
	}
	cards, _ := h.sent()
	if len(cards) != 2 || !strings.HasPrefix(cards[1].Title, "Pack sweep done") {
		t.Errorf("cards = %+v, want the start card and the summary", cards)
	}
}

func TestS1_11_StaleRunningRowReconciledOnTick(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.runner.Log = slog.New(slog.NewTextHandler(logs, nil))
	h.rawExec(t, blockFinishSQL)
	var sleeps []time.Duration
	sleep := h.runner.Sleep
	h.runner.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return sleep(ctx, d)
	}
	run := h.run(t, false, settings())
	if run.Status != sweepstate.Running {
		t.Fatalf("status = %s, want running", run.Status)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second {
		t.Errorf("sleeps = %v, want 1s then 2s", sleeps)
	}
	if cards, refreshed := h.sent(); len(cards) != 1 || len(refreshed) != 0 {
		t.Errorf("cards = %d, refreshed = %v; want only the start card", len(cards), refreshed)
	}
	if !logs.has("ERROR", "sweep finish failed") {
		t.Error("no ERROR sweep finish failed line")
	}

	h.rawExec(t, dropBlockSQL)
	sch := h.restart(t0())
	sch.Tick(t0())
	sch.Wait()
	got, err := h.db.GetSweepRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sweepstate.Crashed {
		t.Errorf("status = %s, want crashed", got.Status)
	}
	if _, refreshed := h.sent(); len(refreshed) != 1 || refreshed[0] != card.SweepSubject(run.ID) {
		t.Errorf("refreshed = %v, want [%s]", refreshed, card.SweepSubject(run.ID))
	}
	if u, uerr := sch.Used(t0()); uerr != nil || !u.Crashed {
		t.Errorf("Used = %+v, %v; want Crashed", u, uerr)
	}
}

func TestS1_11_TickSkipsLiveRun(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.fake.beforeLookup = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	if err := sch.RunNow("manual", true); err != nil {
		t.Fatal(err)
	}
	<-entered
	sch.Tick(t0())
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != sweepstate.Running {
		t.Errorf("runs = %+v, want one still running", runs)
	}
	close(release)
	sch.Wait()
	if runs = h.runs(t); len(runs) != 1 || runs[0].Status != sweepstate.Done {
		t.Errorf("runs = %+v, want one done", runs)
	}
}

func TestS1_11_TickAfterShutdownIsQuiet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.runner.Log = slog.New(slog.NewTextHandler(logs, nil))
	id, err := h.db.CreateSweepRun(context.Background(), "manual", false, t0())
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	cancel()
	sch := sweep.NewScheduler(base, h.runner, h.db)
	sch.Tick(t0())
	got, err := h.db.GetSweepRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sweepstate.Running {
		t.Errorf("status = %s, want running: no reconcile write after shutdown", got.Status)
	}
	if n := logs.count("WARN"); n != 0 {
		t.Errorf("WARNs after shutdown = %d, want 0", n)
	}
}

func TestS1_11_RetryAfterCommittedFinishCountsAsDone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.startErr = card.ErrUntracked
	logs := &lockedBuffer{}
	h.runner.Log = slog.New(slog.NewTextHandler(logs, nil))
	h.rawExec(t, blockFinishSQL)
	sleep := h.runner.Sleep
	h.runner.Sleep = func(ctx context.Context, d time.Duration) error {
		h.rawExec(t, dropBlockSQL)
		runs := h.runs(t)
		if len(runs) != 1 {
			t.Fatalf("runs = %+v", runs)
		}
		if err := h.db.FinishSweepRun(ctx, runs[0].ID, sweepstate.Done, "", db.SweepCounts{}, t0()); err != nil {
			t.Fatal(err)
		}
		return sleep(ctx, d)
	}
	run := h.run(t, false, settings())
	if run.Status != sweepstate.Done {
		t.Errorf("status = %s, want done", run.Status)
	}
	cards, _ := h.sent()
	if len(cards) != 2 {
		t.Errorf("cards = %d, want the start card and one summary", len(cards))
	}
	if n := logs.levelCount("ERROR"); n != 0 {
		t.Errorf("ERROR lines = %d, want 0", n)
	}
}
