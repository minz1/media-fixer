package sweep_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func newScheduled(t *testing.T, s sweep.Settings) (*harness, *sweep.Scheduler) {
	t.Helper()
	h := newHarness(t)
	if err := sweep.SaveSettings(context.Background(), h.db, s, h.clock.Now()); err != nil {
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

func (h *harness) restart(at time.Time) *sweep.Scheduler {
	h.clock.mu.Lock()
	h.clock.t = at
	h.clock.mu.Unlock()
	return sweep.NewScheduler(context.Background(), h.runner, h.db)
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

func TestScheduler_InterruptedRunReRunsAfterRestart(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-time.Hour), settings())
	ctx := context.Background()
	if _, err := h.db.CreateSweepRun(ctx, "schedule", false, t0().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := h.db.InterruptRunningSweeps(ctx); err != nil || n != 1 {
		t.Fatalf("InterruptRunningSweeps = %d, %v", n, err)
	}
	sch := h.restart(t0().Add(20 * time.Minute))
	h.tick(sch, t0().Add(21*time.Minute))
	if n := len(h.runs(t)); n != 1 {
		t.Fatalf("runs 20 min after the interrupted start = %d, want 1 (retry waits an hour)", n)
	}
	h.tick(sch, t0().Add(time.Hour+2*time.Minute))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Status != "done" || runs[1].Status != "interrupted" {
		t.Errorf("runs = %+v, want a fresh done run after the interrupted one", runs)
	}
}

func TestScheduler_MissedSlotRunsOnceAfterRestart(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().AddDate(0, 0, -1), settings())
	tuesday := t0().AddDate(0, 0, 2)
	sch := h.restart(tuesday)
	for m := range 90 {
		h.tick(sch, tuesday.Add(time.Duration(m)*time.Minute))
	}
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Trigger != "schedule" || runs[0].Status != "done" {
		t.Errorf("runs = %+v, want one scheduled run for the missed slot", runs)
	}
}

func TestScheduler_LegacyEnabledRowRunsMissedSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	raw, err := json.Marshal(settings())
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.SetSetting(context.Background(), sweep.SettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	sch := h.restart(t0().AddDate(0, 0, 1))
	h.tick(sch, t0().AddDate(0, 0, 1))
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("runs = %d, want 1 for a row saved before enabled_at existed", n)
	}
}

func TestScheduler_DryRunAfterSlotDoesNotUseIt(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0().Add(-time.Hour), settings())
	h.clock.mu.Lock()
	h.clock.t = t0().Add(time.Minute)
	h.clock.mu.Unlock()
	if err := sch.RunNow("manual", true); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	h.tick(sch, t0().Add(30*time.Minute))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Trigger != "schedule" || runs[1].Trigger != "manual" || !runs[1].Dry {
		t.Errorf("runs = %+v, want the dry run then a scheduled run", runs)
	}
}

func TestSaveSettings_EnabledAt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	save := func(s sweep.Settings, at time.Time) *time.Time {
		t.Helper()
		if err := sweep.SaveSettings(ctx, h.db, s, at); err != nil {
			t.Fatal(err)
		}
		got, err := sweep.LoadSettings(ctx, h.db)
		if err != nil {
			t.Fatal(err)
		}
		return got.EnabledAt
	}
	if at := save(sweep.DefaultSettings(), t0()); at != nil {
		t.Errorf("disabled save: enabled_at = %v, want none", at)
	}
	if at := save(settings(), t0().Add(time.Hour)); at == nil || !at.Equal(t0().Add(time.Hour)) {
		t.Errorf("enabling save: enabled_at = %v, want %v", at, t0().Add(time.Hour))
	}
	if at := save(settings(), t0().Add(2*time.Hour)); at == nil || !at.Equal(t0().Add(time.Hour)) {
		t.Errorf("re-save while enabled: enabled_at = %v, want %v kept", at, t0().Add(time.Hour))
	}
	if at := save(sweep.DefaultSettings(), t0().Add(3*time.Hour)); at != nil {
		t.Errorf("disabling save: enabled_at = %v, want cleared", at)
	}
	raw, err := json.Marshal(settings())
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.SetSetting(ctx, sweep.SettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	if at := save(settings(), t0().Add(4*time.Hour)); at != nil {
		t.Errorf("re-save of a legacy enabled row: enabled_at = %v, want none", at)
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
		if err := sweep.SaveSettings(context.Background(), h.db, s, h.clock.Now()); err != nil {
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
	if err := sweep.SaveSettings(context.Background(), h.db, s, h.clock.Now()); err != nil {
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
	sch = h.restart(wednesday.Add(2 * time.Hour))
	for m := range 60 {
		h.tick(sch, wednesday.Add(2*time.Hour+time.Duration(m)*time.Minute))
	}
	if n := len(h.runs(t)); n != 0 {
		t.Fatalf("a restart after enabling mid-week started %d runs", n)
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
	if err := sweep.SaveSettings(context.Background(), h.db, settings(), h.clock.Now()); err != nil {
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
	failed := h.runs(t)
	if len(failed) != 1 || failed[0].Status != "failed" {
		t.Fatalf("runs = %+v, want one failed run", failed)
	}
	sch = h.restart(t0().Add(30 * time.Minute))
	h.tick(sch, t0().Add(30*time.Minute))
	if n := len(h.runs(t)); n != 1 {
		t.Fatalf("runs 30 min after a failure and a restart = %d, want 1", n)
	}
	retry := failed[0].FinishedAt.Add(time.Hour)
	if next := sch.NextRun(t0().Add(30 * time.Minute)); next == nil || !next.Equal(retry) {
		t.Errorf("NextRun during the backoff = %v, want %v", next, retry)
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

func TestScheduler_FailedDryRunDoesNotDelaySlot(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0().Add(-time.Hour), settings())
	h.fake.seriesDownTil = t0().Add(-time.Minute)
	h.clock.t = t0().Add(-30 * time.Minute)
	if err := sch.RunNow("manual", true); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	h.tick(sch, t0())
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Trigger != "schedule" || runs[0].Status != "done" {
		t.Errorf("runs = %+v, want the failed dry run then a done scheduled run", runs)
	}
}

func TestScheduler_FailedRunRetriesTwicePerSlot(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.fake.seriesDownTil = t0().AddDate(0, 0, 3)
	for m := 0; m <= 8*60; m += 10 {
		if m == 100 {
			sch = h.restart(t0().Add(time.Duration(m) * time.Minute))
		}
		h.tick(sch, t0().Add(time.Duration(m)*time.Minute))
	}
	runs := h.runs(t)
	if len(runs) != 3 {
		t.Fatalf("runs = %+v, want the failed run and two retries", runs)
	}
	for i, r := range runs {
		if r.Status != "failed" {
			t.Errorf("run %d status = %s, want failed", i, r.Status)
		}
		if i < len(runs)-1 && runs[i].StartedAt.Sub(*runs[i+1].FinishedAt) < time.Hour {
			t.Errorf("retry %d started %v after the previous failure, want at least 1h",
				i, runs[i].StartedAt.Sub(*runs[i+1].FinishedAt))
		}
	}
	if next := sch.NextRun(t0().Add(8 * time.Hour)); next == nil || !next.Equal(t0().AddDate(0, 0, 7)) {
		t.Errorf("NextRun after the retries = %v, want %v", next, t0().AddDate(0, 0, 7))
	}
}

func TestScheduler_PruningDryRunsKeepsSlotRun(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.tick(sch, t0())
	h.clock.mu.Lock()
	h.clock.t = t0().Add(time.Hour)
	h.clock.mu.Unlock()
	for range 51 {
		h.run(t, true, settings())
	}
	h.tick(sch, t0().Add(2*time.Hour))
	for _, r := range h.runs(t) {
		if !r.Dry && r.StartedAt.After(t0()) {
			t.Errorf("live run %d at %v, want none after the slot's run", r.ID, r.StartedAt)
		}
	}
}

func interruptedRun(t *testing.T, h *harness, at time.Time, results ...string) {
	t.Helper()
	ctx := context.Background()
	id, err := h.db.CreateSweepRun(ctx, "schedule", false, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		if err = h.db.AddSweepItem(ctx, &db.SweepItem{RunID: id, Target: "season", SeriesID: 1, SeriesTitle: "Show",
			Result: res, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.db.InterruptRunningSweeps(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestScheduler_InterruptedRunsCapped(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-time.Hour), settings())
	for _, at := range []time.Time{t0().Add(time.Minute), t0().Add(time.Hour + 2*time.Minute), t0().Add(2*time.Hour + 3*time.Minute)} {
		interruptedRun(t, h, at)
	}
	sch := h.restart(t0().Add(3*time.Hour + 5*time.Minute))
	h.tick(sch, t0().Add(3*time.Hour+5*time.Minute))
	if n := len(h.runs(t)); n != 3 {
		t.Errorf("runs = %d, want 3 (two retries used, no third)", n)
	}
	if next := sch.NextRun(t0().Add(3 * time.Hour)); next == nil || !next.Equal(t0().AddDate(0, 0, 7)) {
		t.Errorf("NextRun = %v, want the next slot %v", next, t0().AddDate(0, 0, 7))
	}
}

func TestScheduler_RetryGetsRemainingBudget(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-time.Hour), settings())
	interruptedRun(t, h, t0().Add(time.Minute), "imported", "imported", "failed")
	sch := h.restart(t0().Add(time.Hour + 2*time.Minute))
	h.tick(sch, t0().Add(time.Hour+2*time.Minute))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Counts.MaxGrabs != settings().MaxGrabs-3 {
		t.Errorf("runs = %+v, want a retry with max_grabs %d", runs, settings().MaxGrabs-3)
	}
}

func TestScheduler_ZeroBudgetMakesNoGrab(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-time.Hour), settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	used := make([]string, settings().MaxGrabs)
	for i := range used {
		used[i] = "imported"
	}
	interruptedRun(t, h, t0().Add(time.Minute), used...)
	sch := h.restart(t0().Add(time.Hour + 2*time.Minute))
	h.tick(sch, t0().Add(time.Hour+2*time.Minute))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Status != "done" || runs[0].Counts.MaxGrabs != 0 {
		t.Errorf("runs = %+v, want a done retry with max_grabs 0", runs)
	}
	if n := h.fake.countWrites("POST /release"); n != 0 {
		t.Errorf("POST /release count = %d, want 0", n)
	}
}

func TestRunNow_IgnoresSlotGrabs(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-time.Hour), settings())
	used := make([]string, settings().MaxGrabs)
	for i := range used {
		used[i] = "imported"
	}
	interruptedRun(t, h, t0().Add(time.Minute), used...)
	sch := h.restart(t0().Add(2 * time.Hour))
	if err := sch.RunNow("manual", false); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	if runs := h.runs(t); runs[0].Counts.MaxGrabs != settings().MaxGrabs {
		t.Errorf("manual run max_grabs = %d, want the full %d", runs[0].Counts.MaxGrabs, settings().MaxGrabs)
	}
}

func TestSaveSettings_SlotChangeStampsEnabledAt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	s := settings()
	if err := sweep.SaveSettings(ctx, h.db, s, t0().AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	s.HourUTC = 10
	if err := sweep.SaveSettings(ctx, h.db, s, t0()); err != nil {
		t.Fatal(err)
	}
	got, err := sweep.LoadSettings(ctx, h.db)
	if err != nil || got.EnabledAt == nil || !got.EnabledAt.Equal(t0()) {
		t.Fatalf("enabled_at after a slot change = %v, %v; want %v", got.EnabledAt, err, t0())
	}
	s.MaxGrabs = 2
	if err = sweep.SaveSettings(ctx, h.db, s, t0().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err = sweep.LoadSettings(ctx, h.db); err != nil || !got.EnabledAt.Equal(t0()) {
		t.Errorf("enabled_at after a non-slot change = %v, %v; want %v", got.EnabledAt, err, t0())
	}
}

func TestScheduler_SlotMovedToPassedLaterHourWaits(t *testing.T) {
	t.Parallel()
	early := settings()
	early.HourUTC = 10
	h, sch := scheduledAt(t, t0().AddDate(0, 0, -1), early)
	h.tick(sch, t0().Add(-2*time.Hour))
	if err := sweep.SaveSettings(context.Background(), h.db, settings(), t0().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.tick(sch, t0().Add(2*time.Hour+time.Minute))
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("runs = %d, want 1 (no second run the same week)", n)
	}
	if next := sch.NextRun(t0().Add(2*time.Hour + time.Minute)); next == nil || !next.Equal(t0().AddDate(0, 0, 7)) {
		t.Errorf("NextRun = %v, want %v", next, t0().AddDate(0, 0, 7))
	}
}

func TestScheduler_SlotMovedLaterRunsThisWeek(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0().Add(-time.Hour), settings())
	h.tick(sch, t0())
	moved := settings()
	moved.Weekday = 3
	if err := sweep.SaveSettings(context.Background(), h.db, moved, t0().AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	h.tick(sch, t0().AddDate(0, 0, 3).Add(time.Minute))
	if n := len(h.runs(t)); n != 2 {
		t.Errorf("runs = %d, want 2 (the moved slot runs on Wednesday)", n)
	}
}

func TestScheduler_CancelLiveStopsRun(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	h.fake.beforePost = func() { sch.CancelLive(sweep.ErrDisabled) }
	if err := sch.RunNow("manual", false); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != "interrupted" || runs[0].Reason != sweep.ErrDisabled.Error() {
		t.Errorf("runs = %+v, want one run interrupted with %q", runs, sweep.ErrDisabled.Error())
	}
}

func TestScheduler_CancelLiveSparesDryRun(t *testing.T) {
	t.Parallel()
	h, sch := newScheduled(t, settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	h.fake.beforeLookup = func() { sch.CancelLive(sweep.ErrDisabled) }
	if err := sch.RunNow("manual", true); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	if runs := h.runs(t); len(runs) != 1 || runs[0].Status != "done" {
		t.Errorf("runs = %+v, want the dry run done", runs)
	}
}

func TestScheduler_ShutdownStillInterrupted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := sweep.SaveSettings(context.Background(), h.db, settings(), h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	signal := errors.New("terminated signal received")
	base, cancel := context.WithCancelCause(context.Background())
	sch := sweep.NewScheduler(base, h.runner, h.db)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	h.fake.beforePost = func() { cancel(signal) }
	if err := sch.RunNow("manual", false); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != "interrupted" || runs[0].Reason != signal.Error() {
		t.Errorf("runs = %+v, want one run interrupted with %q", runs, signal.Error())
	}
}
