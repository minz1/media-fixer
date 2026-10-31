package sweep_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func scheduledAt(t *testing.T, at time.Time, s sweep.Settings) (*harness, *sweep.Scheduler) {
	t.Helper()
	h := newHarness(t)
	h.clock.t = at
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

func TestScheduler_NoRunRightAfterEnabling(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	h.tick(sch, t0())
	h.tick(sch, t0().Add(5*time.Hour+59*time.Minute))
	if n := len(h.runs(t)); n != 0 {
		t.Fatalf("runs before the first interval = %d, want 0", n)
	}
	sch = h.restart(t0().Add(6 * time.Hour))
	h.tick(sch, t0().Add(6*time.Hour))
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Trigger != "schedule" || runs[0].Status != "done" {
		t.Fatalf("runs = %+v, want one done scheduled run", runs)
	}
}

func TestScheduler_SlotFromLastRun(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-24*time.Hour), settings())
	finishedRun(t, h, t0(), "done", db.SweepCounts{})
	sch := h.restart(t0().Add(time.Hour))
	if next := sch.NextRun(t0().Add(time.Hour)); next == nil || !next.Equal(t0().Add(6*time.Hour)) {
		t.Errorf("NextRun = %v, want %v", next, t0().Add(6*time.Hour))
	}
	late := t0().Add(20 * time.Hour)
	h.tick(sch, late)
	h.tick(sch, late.Add(time.Hour))
	if n := len(h.runs(t)); n != 2 {
		t.Fatalf("runs = %d, want the old run and one for the missed slots", n)
	}
	if next := sch.NextRun(late.Add(time.Hour)); next == nil || !next.Equal(late.Add(6*time.Hour)) {
		t.Errorf("NextRun = %v, want %v", next, late.Add(6*time.Hour))
	}
}

func TestScheduler_EnabledAtAfterLastRun(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-24*time.Hour), sweep.DefaultSettings())
	finishedRun(t, h, t0(), "done", db.SweepCounts{})
	if err := sweep.SaveSettings(context.Background(), h.db, settings(), t0().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	sch := h.restart(t0().Add(3 * time.Hour))
	if next := sch.NextRun(t0().Add(3 * time.Hour)); next == nil || !next.Equal(t0().Add(8*time.Hour)) {
		t.Errorf("NextRun = %v, want %v", next, t0().Add(8*time.Hour))
	}
}

func TestScheduler_PausedSlotRunsAfterUnpause(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	due := t0().Add(6 * time.Hour)
	h.setPaused(t, true)
	h.tick(sch, due)
	if n := len(h.runs(t)); n != 0 || len(*h.dms) != 0 {
		t.Fatalf("paused slot: runs %d, dms %v", n, *h.dms)
	}
	if next := sch.NextRun(due.Add(time.Hour)); next == nil || !next.Equal(due) {
		t.Errorf("NextRun while the slot is pending = %v, want %v", next, due)
	}
	h.setPaused(t, false)
	h.tick(sch, due.Add(2*time.Hour))
	h.tick(sch, due.Add(3*time.Hour))
	if n := len(h.runs(t)); n != 1 {
		t.Errorf("runs after unpause = %d, want 1", n)
	}
}

func TestScheduler_RefusedCountsAsSlotRun(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	h.fake.flags.AutoRedownloadFailed = true
	h.fake.flags.AutoRedownloadFailedFromInteractiveSearch = true
	due := t0().Add(6 * time.Hour)
	h.tick(sch, due)
	h.tick(sch, due.Add(time.Hour))
	h.tick(sch, due.Add(5*time.Hour))
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != "refused" || len(*h.dms) != 1 {
		t.Errorf("runs = %+v, dms = %v, want one refused run and one DM", runs, *h.dms)
	}
}

func TestScheduler_InterruptedRunReRunsAfterRestart(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-7*time.Hour), settings())
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

func TestScheduler_OldEnabledRowDueNow(t *testing.T) {
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
	h, sch := scheduledAt(t, t0().Add(-7*time.Hour), settings())
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
	budgets := settings()
	budgets.MaxGrabs = 2
	if at := save(budgets, t0().Add(3*time.Hour)); at == nil || !at.Equal(t0().Add(time.Hour)) {
		t.Errorf("budgets-only change: enabled_at = %v, want %v kept", at, t0().Add(time.Hour))
	}
	slower := settings()
	slower.IntervalHours = 12
	if at := save(slower, t0().Add(4*time.Hour)); at == nil || !at.Equal(t0().Add(4*time.Hour)) {
		t.Errorf("interval change: enabled_at = %v, want %v", at, t0().Add(4*time.Hour))
	}
	if at := save(sweep.DefaultSettings(), t0().Add(5*time.Hour)); at != nil {
		t.Errorf("disabling save: enabled_at = %v, want cleared", at)
	}
	raw, err := json.Marshal(settings())
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.SetSetting(ctx, sweep.SettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	if at := save(settings(), t0().Add(6*time.Hour)); at != nil {
		t.Errorf("re-save of a legacy enabled row: enabled_at = %v, want none", at)
	}
}

func TestSaveSettings_OverwritesCorruptRow(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{not json`, `[]`, `{"max_grabs":"x"}`, `{"enabled_at":"garbage"}`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			ctx := context.Background()
			if err := h.db.SetSetting(ctx, sweep.SettingsKey, raw); err != nil {
				t.Fatal(err)
			}
			if err := sweep.SaveSettings(ctx, h.db, settings(), t0()); err != nil {
				t.Fatalf("SaveSettings over %q: %v", raw, err)
			}
			got, err := sweep.LoadSettings(ctx, h.db)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Enabled {
				t.Error("saved settings not enabled")
			}
			if got.EnabledAt == nil || !got.EnabledAt.Equal(t0()) {
				t.Errorf("enabled_at = %v, want %v", got.EnabledAt, t0())
			}
		})
	}
}

func TestScheduler_DisabledNeverFires(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), sweep.DefaultSettings())
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
	h, sch := scheduledAt(t, t0(), settings())
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
	h, sch := scheduledAt(t, t0(), settings())
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
	h, sch := scheduledAt(t, t0(), sweep.DefaultSettings())
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

func TestScheduler_FailedRunRetriesAfterAnHour(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0().Add(-6*time.Hour), settings())
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
	h, sch := scheduledAt(t, t0().Add(-6*time.Hour), settings())
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
	h, sch := scheduledAt(t, t0().Add(-6*time.Hour), settings())
	h.fake.seriesDownTil = t0().AddDate(0, 0, 3)
	for m := 0; m < 6*60; m += 10 {
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
	if next := sch.NextRun(t0().Add(5 * time.Hour)); next == nil || !next.Equal(t0().Add(6*time.Hour)) {
		t.Errorf("NextRun after the retries = %v, want %v", next, t0().Add(6*time.Hour))
	}
	h.tick(sch, t0().Add(6*time.Hour))
	if n := len(h.runs(t)); n != 4 {
		t.Errorf("runs at the next slot = %d, want 4", n)
	}
}

func TestScheduler_OldRowFailuresWaitForNextSlot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	raw, err := json.Marshal(settings())
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.SetSetting(context.Background(), sweep.SettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		finishedRun(t, h, t0().Add(time.Duration(i)*time.Hour), "failed", db.SweepCounts{})
	}
	sch := h.restart(t0().Add(3 * time.Hour))
	for m := 3 * 60; m < 6*60; m += 10 {
		h.tick(sch, t0().Add(time.Duration(m)*time.Minute))
	}
	if n := len(h.runs(t)); n != 3 {
		t.Fatalf("runs inside the slot = %d, want only the 3 failures", n)
	}
	if next := sch.NextRun(t0().Add(3 * time.Hour)); next == nil || !next.Equal(t0().Add(6*time.Hour)) {
		t.Errorf("NextRun = %v, want %v", next, t0().Add(6*time.Hour))
	}
	h.tick(sch, t0().Add(6*time.Hour))
	if n := len(h.runs(t)); n != 4 {
		t.Errorf("runs at the next slot = %d, want 4", n)
	}
}

func TestScheduler_NextRunFallback(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sweep.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err = sweep.SaveSettings(context.Background(), d, settings(), t0()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.Exec(`DROP TABLE sweep_runs`); err != nil {
		t.Fatal(err)
	}
	sch := sweep.NewScheduler(context.Background(), &sweep.Runner{DB: d}, d)
	if next := sch.NextRun(t0().Add(time.Hour)); next == nil || !next.Equal(t0().Add(7*time.Hour)) {
		t.Errorf("NextRun on a DB error = %v, want %v", next, t0().Add(7*time.Hour))
	}
}

func TestScheduler_PruningDryRunsKeepsSlotRun(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0().Add(-6*time.Hour), settings())
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
			ReleaseTitle: "Show.S01", Result: res, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.db.InterruptRunningSweeps(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestScheduler_InterruptedRunsCapped(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-7*time.Hour), settings())
	for _, at := range []time.Time{t0().Add(time.Minute), t0().Add(time.Hour + 2*time.Minute), t0().Add(2*time.Hour + 3*time.Minute)} {
		interruptedRun(t, h, at)
	}
	sch := h.restart(t0().Add(3*time.Hour + 5*time.Minute))
	h.tick(sch, t0().Add(3*time.Hour+5*time.Minute))
	if n := len(h.runs(t)); n != 3 {
		t.Errorf("runs = %d, want 3 (two retries used, no third)", n)
	}
	if next := sch.NextRun(t0().Add(3 * time.Hour)); next == nil || !next.Equal(t0().Add(5*time.Hour)) {
		t.Errorf("NextRun = %v, want the next slot %v", next, t0().Add(5*time.Hour))
	}
}

func TestScheduler_RetryGetsRemainingBudget(t *testing.T) {
	t.Parallel()
	h, _ := scheduledAt(t, t0().Add(-7*time.Hour), settings())
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
	h, _ := scheduledAt(t, t0().Add(-7*time.Hour), settings())
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

func TestScheduler_CancelLiveStopsRun(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
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
	h, sch := scheduledAt(t, t0(), settings())
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

func finishedRun(t *testing.T, h *harness, at time.Time, status string, c db.SweepCounts) {
	t.Helper()
	ctx := context.Background()
	id, err := h.db.CreateSweepRun(ctx, "schedule", false, at)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.FinishSweepRun(ctx, id, status, "", c, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestScheduler_RetryGetsRemainingSearches(t *testing.T) {
	t.Parallel()
	s := settings()
	s.MaxSearches = 50
	h, _ := scheduledAt(t, t0().Add(-7*time.Hour), s)
	finishedRun(t, h, t0().Add(time.Minute), "interrupted", db.SweepCounts{Searched: 20})
	sch := h.restart(t0().Add(time.Hour + 2*time.Minute))
	h.tick(sch, t0().Add(time.Hour+2*time.Minute))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Counts.MaxSearches != 30 {
		t.Errorf("runs = %+v, want a retry with max_searches 30", runs)
	}
}

func TestScheduler_RetryAfterCrashMakesNoSearches(t *testing.T) {
	t.Parallel()
	s := settings()
	s.MaxSearches = 50
	h, _ := scheduledAt(t, t0().Add(-7*time.Hour), s)
	interruptedRun(t, h, t0().Add(time.Minute))
	sch := h.restart(t0().Add(time.Hour + 2*time.Minute))
	h.tick(sch, t0().Add(time.Hour+2*time.Minute))
	runs := h.runs(t)
	if len(runs) != 2 || runs[0].Counts.MaxSearches != 0 {
		t.Errorf("runs = %+v, want a retry with max_searches 0 (the crashed run's searches are unknown)", runs)
	}
}

func TestSaveSettings_PartlyReadableEnabledRowGetsFreshEnabledAt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	raw := fmt.Sprintf(
		`{"enabled":true,"weekday":%d,"hour_utc":%d,"enabled_at":"2020-01-01T00:00:00Z","max_grabs":"x"}`,
		settings().Weekday, settings().HourUTC,
	)
	if err := h.db.SetSetting(ctx, sweep.SettingsKey, raw); err != nil {
		t.Fatal(err)
	}
	if err := sweep.SaveSettings(ctx, h.db, settings(), t0()); err != nil {
		t.Fatal(err)
	}
	got, err := sweep.LoadSettings(ctx, h.db)
	if err != nil || got.EnabledAt == nil || !got.EnabledAt.Equal(t0()) {
		t.Errorf("enabled_at = %v, %v; want %v (the unreadable row's old stamp must not survive)",
			got.EnabledAt, err, t0())
	}
}

func TestScheduler_CorruptEnabledRowNeverFires(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.db.SetSetting(context.Background(), sweep.SettingsKey, `{"enabled":true,"max_grabs":"x"}`); err != nil {
		t.Fatal(err)
	}
	sch := sweep.NewScheduler(context.Background(), h.runner, h.db)
	for d := range 8 {
		h.tick(sch, t0().AddDate(0, 0, d))
	}
	if n := len(h.runs(t)); n != 0 {
		t.Errorf("runs = %d, want none while the settings row is unreadable", n)
	}
	if next := sch.NextRun(t0()); next != nil {
		t.Errorf("NextRun = %v, want nil", next)
	}
}
