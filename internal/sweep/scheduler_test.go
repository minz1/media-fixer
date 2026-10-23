package sweep_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func newScheduled(t *testing.T, s sweep.Settings) (*harness, *sweep.Scheduler) {
	t.Helper()
	h := newHarness(t)
	if err := sweep.SaveSettings(context.Background(), h.db, s); err != nil {
		t.Fatal(err)
	}
	return h, sweep.NewScheduler(context.Background(), h.runner, h.db)
}

func (h *harness) runs(t *testing.T) []db.SweepRun {
	t.Helper()
	runs, err := h.db.ListSweepRuns(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (h *harness) tick(sch *sweep.Scheduler, at time.Time) {
	h.clock.mu.Lock()
	h.clock.t = at
	h.clock.mu.Unlock()
	sch.Tick(at)
	sch.Wait()
}

func TestScheduler_FiresOncePerSlot(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.tick(sch, t0())
	for m := 1; m <= 60; m++ {
		h.tick(sch, t0().Add(time.Duration(m)*time.Minute))
	}
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Trigger != "schedule" || runs[0].Status != "done" {
		t.Fatalf("runs = %+v, want one done scheduled run", runs)
	}
	if next := sch.NextRun(t0().Add(time.Hour)); next == nil || !next.Equal(t0().AddDate(0, 0, 7)) {
		t.Errorf("NextRun = %v, want %v", next, t0().AddDate(0, 0, 7))
	}
	h.tick(sch, t0().AddDate(0, 0, 7))
	if n := len(h.runs(t)); n != 2 {
		t.Errorf("runs after next slot = %d, want 2", n)
	}
}

func TestScheduler_PausedSlotRunsAfterUnpause(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.setPaused(t, true)
	h.tick(sch, t0())
	if n := len(h.runs(t)); n != 0 || len(*h.dms) != 0 {
		t.Fatalf("paused slot: runs %d, dms %v", n, *h.dms)
	}
	if next := sch.NextRun(t0().Add(time.Hour)); next == nil || !next.Equal(t0()) {
		t.Errorf("NextRun while the slot is pending = %v, want %v", next, t0())
	}
	h.setPaused(t, false)
	tuesday := t0().AddDate(0, 0, 2)
	h.tick(sch, tuesday)
	h.tick(sch, tuesday.Add(time.Hour))
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("runs after unpause = %d, want 1", n)
	}
}

func TestScheduler_RefusedCountsAsSlotRun(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.fake.flags.AutoRedownloadFailed = true
	h.fake.flags.AutoRedownloadFailedFromInteractiveSearch = true
	h.tick(sch, t0())
	h.tick(sch, t0().Add(time.Hour))
	h.tick(sch, t0().AddDate(0, 0, 3))
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != "refused" || len(*h.dms) != 1 {
		t.Errorf("runs = %+v, dms = %v, want one refused run and one DM", runs, *h.dms)
	}
}

func TestScheduler_InterruptedRunDoesNotConsumeSlot(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	ctx := context.Background()
	if _, err := h.db.CreateSweepRun(ctx, "schedule", false, t0().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := h.db.InterruptRunningSweeps(ctx); err != nil || n != 1 {
		t.Fatalf("InterruptRunningSweeps = %d, %v", n, err)
	}
	h.tick(sch, t0().Add(10*time.Minute))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Status != "done" || runs[1].Status != "interrupted" {
		t.Errorf("runs = %+v, want a fresh done run after the interrupted one", runs)
	}
}

func TestScheduler_DisabledNeverFires(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, sweep.DefaultSettings())
	for d := range 8 {
		h.tick(sch, t0().AddDate(0, 0, d))
	}
	if n := len(h.runs(t)); n != 0 {
		t.Errorf("disabled sweep ran %d times", n)
	}
	if next := sch.NextRun(t0()); next != nil {
		t.Errorf("NextRun = %v, want nil when disabled", next)
	}
}

func TestRunNow_ActiveAndPaused(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	entered, release := make(chan struct{}), make(chan struct{})
	h.fake.beforePost = func() {
		close(entered)
		<-release
	}
	if err := sch.RunNow("manual", false); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := sch.RunNow("manual", true); !errors.Is(err, sweep.ErrRunActive) {
		t.Errorf("second RunNow = %v, want ErrRunActive", err)
	}
	close(release)
	sch.Wait()
	h.setPaused(t, true)
	if err := sch.RunNow("manual", false); !errors.Is(err, sweep.ErrPaused) {
		t.Errorf("paused RunNow = %v, want ErrPaused", err)
	}
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Trigger != "manual" || runs[0].Status != "done" {
		t.Errorf("runs = %+v, want the one manual run", runs)
	}
}

func TestSettingsChangeDoesNotAffectActiveRun(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12, 13)...)
	h.fake.addSeries(2, "Other", missingEps(2, 1, 31, 32)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.setReleases(seasonKey(2, 1), seasonRel("g2", "Other S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	h.fake.plan("g2", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	saved := false
	h.fake.beforePost = func() {
		if saved {
			return
		}
		saved = true
		s := sweep.DefaultSettings()
		s.MaxGrabs = 1
		if err := sweep.SaveSettings(context.Background(), h.db, s); err != nil {
			t.Error(err)
		}
	}
	if err := sch.RunNow("manual", false); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Counts.Grabs != 2 || runs[0].Counts.MaxGrabs != 5 {
		t.Fatalf("runs = %+v, want one run with 2 of 5 grabs", runs)
	}
	next, err := sweep.LoadSettings(context.Background(), h.db)
	if err != nil || next.MaxGrabs != 1 {
		t.Errorf("stored settings = %+v, %v; the next run should use MaxGrabs 1", next, err)
	}
}

func TestRunNow_LiveRefusedWhileDisabled(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, sweep.DefaultSettings())
	if err := sch.RunNow("manual", false); !errors.Is(err, sweep.ErrDisabled) {
		t.Fatalf("live RunNow while disabled = %v, want ErrDisabled", err)
	}
	sch.Wait()
	if n := len(h.runs(t)); n != 0 {
		t.Fatalf("refused live run created %d runs", n)
	}
	if err := sch.RunNow("manual", true); err != nil {
		t.Fatalf("dry RunNow while disabled = %v", err)
	}
	sch.Wait()
	if runs := h.runs(t); len(runs) != 1 || !runs[0].Dry {
		t.Errorf("runs = %+v, want one dry run", runs)
	}
}

func scheduledAt(t *testing.T, at time.Time, s sweep.Settings) (*harness, *sweep.Scheduler) {
	t.Helper()
	h := newHarness(t)
	h.clock.t = at
	if err := sweep.SaveSettings(context.Background(), h.db, s); err != nil {
		t.Fatal(err)
	}
	return h, sweep.NewScheduler(context.Background(), h.runner, h.db)
}

func TestScheduler_FirstEnableWaitsForSlot(t *testing.T) {
	t.Parallel()
	wednesday := t0().AddDate(0, 0, 3)
	h, sch := scheduledAt(t, wednesday, settings())
	for m := range 90 {
		h.tick(sch, wednesday.Add(time.Duration(m)*time.Minute))
	}
	if n := len(h.runs(t)); n != 0 {
		t.Fatalf("enabling mid-week with no prior run started %d runs", n)
	}
	if next := sch.NextRun(wednesday); next == nil || !next.Equal(t0().AddDate(0, 0, 7)) {
		t.Errorf("NextRun = %v, want %v", next, t0().AddDate(0, 0, 7))
	}
	h.tick(sch, t0().AddDate(0, 0, 7))
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("runs at the slot = %d, want 1", n)
	}
}

func TestScheduler_EnableAfterOldDryRunWaitsForSlot(t *testing.T) {
	t.Parallel()
	wednesday := t0().AddDate(0, 0, 3)
	h, sch := scheduledAt(t, wednesday, settings())
	ctx := context.Background()
	id, err := h.db.CreateSweepRun(ctx, "manual", true, t0().AddDate(0, 0, -2))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.FinishSweepRun(ctx, id, "done", "", db.SweepCounts{}, t0().AddDate(0, 0, -2)); err != nil {
		t.Fatal(err)
	}
	h.tick(sch, wednesday.Add(time.Minute))
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("runs = %d, want only the old dry run", n)
	}
}

func TestScheduler_EnabledJustBeforeSlotFires(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0().Add(-time.Hour), sweep.DefaultSettings())
	h.tick(sch, t0().Add(-time.Minute))
	if err := sweep.SaveSettings(context.Background(), h.db, settings()); err != nil {
		t.Fatal(err)
	}
	h.tick(sch, t0())
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("runs at the slot = %d, want 1", n)
	}
}

func TestScheduler_FailedRunRetriesAfterAnHour(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.fake.seriesDownTil = t0().AddDate(0, 0, 1)
	h.tick(sch, t0())
	if runs := h.runs(t); len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	h.tick(sch, t0().Add(30*time.Minute))
	if n := len(h.runs(t)); n != 1 {
		t.Fatalf("runs 30 min after a failure = %d, want 1", n)
	}
	h.fake.mu.Lock()
	h.fake.seriesDownTil = time.Time{}
	h.fake.mu.Unlock()
	h.tick(sch, t0().Add(2*time.Hour))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Status != "done" {
		t.Errorf("runs = %+v, want a done retry after the failed run", runs)
	}
}
