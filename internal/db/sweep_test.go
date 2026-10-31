package db_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func sweepT0() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }

func TestMigration16_Tables(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	got := rawQuery(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE
		(type = 'table' AND name IN ('sweep_runs', 'sweep_items', 'sweep_refused')) OR
		(type = 'index' AND name IN ('sweep_items_run', 'sweep_items_target', 'sweep_refused_key'))`)
	if got != "6" {
		t.Errorf("sweep tables and indexes = %s, want 6", got)
	}
}

func TestSweepRunLifecycle(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	id, err := d.CreateSweepRun(ctx, "manual", true, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	it := &db.SweepItem{RunID: id, Target: "s:7:2", SeriesID: 7, SeriesTitle: "Andor", Season: new(2),
		Episodes: "1,2,3", ReleaseTitle: "Andor.S02.1080p", Indexer: "Comet", Result: "grabbing",
		LookupResults: new(1200), LookupMS: new(4500), At: sweepT0().Add(time.Minute)}
	if err = d.AddSweepItem(ctx, it); err != nil {
		t.Fatal(err)
	}
	if it.ID == 0 {
		t.Fatal("AddSweepItem did not set ID")
	}
	if err = d.UpdateSweepItem(ctx, it.ID, "downloading", "ABC123"); err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateSweepItem(ctx, it.ID, "imported", ""); err != nil {
		t.Fatal(err)
	}
	anime := &db.SweepItem{RunID: id, Target: "e:99", SeriesID: 8, SeriesTitle: "Frieren", Result: "no_pack",
		At: sweepT0().Add(2 * time.Minute)}
	if err = d.AddSweepItem(ctx, anime); err != nil {
		t.Fatal(err)
	}
	counts := db.SweepCounts{Grabs: 1, Lookups: 2, Imported: 1, NoPack: 1, MaxGrabs: 5, MaxLookups: 60}
	if err = d.FinishSweepRun(ctx, id, "done", "", counts, sweepT0().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	runs, err := d.ListSweepRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	r := runs[0]
	if r.ID != id || r.Trigger != "manual" || !r.Dry || r.Status != "done" || r.Counts != counts {
		t.Errorf("run = %+v", r)
	}
	if !r.StartedAt.Equal(sweepT0()) || r.FinishedAt == nil || !r.FinishedAt.Equal(sweepT0().Add(time.Hour)) {
		t.Errorf("run times = %v, %v", r.StartedAt, r.FinishedAt)
	}
	if len(r.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(r.Items))
	}
	want0 := *it
	want0.Result, want0.DownloadID = "imported", "ABC123"
	assertSweepItem(t, r.Items[0], want0)
	assertSweepItem(t, r.Items[1], *anime)
}

func assertSweepItem(t *testing.T, got, want db.SweepItem) {
	t.Helper()
	if !got.At.Equal(want.At) {
		t.Errorf("item %d at = %v, want %v", want.ID, got.At, want.At)
	}
	got.At = want.At
	if !reflect.DeepEqual(got, want) {
		t.Errorf("item = %+v, want %+v", got, want)
	}
}

func TestListSweepRuns_NewestFirstWithLimit(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	var last int64
	for i := range 3 {
		id, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0().Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		last = id
	}
	runs, err := d.ListSweepRuns(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != last || runs[0].Status != "running" || runs[0].FinishedAt != nil {
		t.Errorf("runs = %+v", runs)
	}
}

func TestInterruptRunningSweeps(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	done, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishSweepRun(ctx, done, "done", "", db.SweepCounts{}, sweepT0().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = d.CreateSweepRun(ctx, "schedule", false, sweepT0().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := d.InterruptRunningSweeps(ctx)
	if err != nil || n != 1 {
		t.Fatalf("InterruptRunningSweeps = %d, %v; want 1", n, err)
	}
	runs, err := d.ListSweepRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != "interrupted" || runs[1].Status != "done" {
		t.Errorf("statuses = %s, %s", runs[0].Status, runs[1].Status)
	}
	if n, err = d.InterruptRunningSweeps(ctx); err != nil || n != 0 {
		t.Errorf("second InterruptRunningSweeps = %d, %v; want 0", n, err)
	}
}

func TestPruneSweepRuns_CascadesItems(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	for i := range 60 {
		at := sweepT0().Add(time.Duration(i) * time.Hour)
		var id int64
		if id, err = d.CreateSweepRun(ctx, "schedule", false, at); err != nil {
			t.Fatal(err)
		}
		if err = d.AddSweepItem(ctx, &db.SweepItem{RunID: id, Target: "s:1:1", SeriesID: 1,
			SeriesTitle: "X", Result: "no_pack", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.PruneSweepRuns(ctx, 50, sweepT0().Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, err := d.ListSweepRuns(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 55 {
		t.Fatalf("runs = %d, want 55: rows started after the cutoff stay", len(runs))
	}
	if err = d.PruneSweepRuns(ctx, 50, sweepT0().Add(100*time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, err = d.ListSweepRuns(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 50 || !runs[49].StartedAt.Equal(sweepT0().Add(10*time.Hour)) {
		t.Fatalf("runs = %d, oldest kept %v", len(runs), runs[len(runs)-1].StartedAt)
	}
	if got := rawQuery(t, path, `SELECT COUNT(*) FROM sweep_items`); got != "50" {
		t.Errorf("items = %s, want 50", got)
	}
}

func TestNoPackTargets_IgnoresDryAndOld(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	add := func(dry bool, target, result string, at time.Time) {
		t.Helper()
		id, err := d.CreateSweepRun(ctx, "schedule", dry, at)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.AddSweepItem(ctx, &db.SweepItem{RunID: id, Target: target, SeriesID: 1,
			SeriesTitle: "X", Result: result, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	since := sweepT0().Add(-30 * 24 * time.Hour)
	add(false, "s:1:1", "no_pack", sweepT0().Add(-10*24*time.Hour))
	add(false, "e:5", "no_pack", sweepT0().Add(-29*24*time.Hour))
	add(true, "s:1:2", "no_pack", sweepT0().Add(-24*time.Hour))
	add(false, "s:1:3", "no_pack", sweepT0().Add(-31*24*time.Hour))
	add(false, "s:1:4", "failed", sweepT0().Add(-24*time.Hour))

	got, err := d.NoPackTargets(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["s:1:1"] || !got["e:5"] {
		t.Errorf("NoPackTargets = %v, want s:1:1 and e:5", got)
	}
}

func TestSweepRefused_Window(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	recent := &db.SweepRefused{TitleKey: "andor s02", ReleaseTitle: "Andor.S02", Indexer: "Comet",
		RefusedAt: sweepT0().Add(-24 * time.Hour), Reason: "500: torrent_blocked"}
	old := &db.SweepRefused{TitleKey: "old s01", ReleaseTitle: "Old.S01", Indexer: "1337x",
		RefusedAt: sweepT0().Add(-8 * 24 * time.Hour), Reason: "504"}
	for _, r := range []*db.SweepRefused{recent, old} {
		if err := d.AddSweepRefused(ctx, r); err != nil {
			t.Fatal(err)
		}
		if r.ID == 0 {
			t.Fatal("AddSweepRefused did not set ID")
		}
	}
	since := sweepT0().Add(-7 * 24 * time.Hour)
	list, err := d.ListSweepRefused(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != recent.ID || list[0].Indexer != "Comet" ||
		list[0].Reason != recent.Reason || !list[0].RefusedAt.Equal(recent.RefusedAt) {
		t.Errorf("ListSweepRefused = %+v", list)
	}
	if err = d.DeleteSweepRefused(ctx, recent.ID); err != nil {
		t.Fatal(err)
	}
	if list, err = d.ListSweepRefused(ctx, since); err != nil || len(list) != 0 {
		t.Errorf("ListSweepRefused after delete = %v, %v", list, err)
	}
}

func TestLastSweepRunStart_ExcludesStatusesAndDryRuns(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	exclude := []string{"paused", "interrupted"}

	if _, err := d.LastSweepRunStart(ctx, exclude); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("empty LastSweepRunStart err = %v, want ErrNotFound", err)
	}
	for i, status := range []string{"refused", "done", "paused", "interrupted"} {
		id, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0().Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err = d.FinishSweepRun(
			ctx,
			id,
			status,
			"",
			db.SweepCounts{},
			sweepT0().Add(time.Duration(i)*time.Hour),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.CreateSweepRun(ctx, "manual", true, sweepT0().Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := d.LastSweepRunStart(ctx, exclude)
	if err != nil || got == nil || !got.Equal(sweepT0().Add(time.Hour)) {
		t.Errorf("LastSweepRunStart = %v, %v; want %v", got, err, sweepT0().Add(time.Hour))
	}
	got, err = d.LastSweepRunStart(ctx, nil)
	if err != nil || got == nil || !got.Equal(sweepT0().Add(3*time.Hour)) {
		t.Errorf("LastSweepRunStart(nil) = %v, %v", got, err)
	}
}

func TestRetryableSweepRunsSince(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	for _, r := range []struct {
		hours  int
		dry    bool
		status string
	}{{-1, false, "failed"}, {1, false, "failed"}, {2, true, "failed"}, {3, false, "done"}, {4, false, "interrupted"}} {
		at := sweepT0().Add(time.Duration(r.hours) * time.Hour)
		id, err := d.CreateSweepRun(ctx, "schedule", r.dry, at)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.FinishSweepRun(ctx, id, r.status, "", db.SweepCounts{}, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0().Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InterruptRunningSweeps(ctx); err != nil {
		t.Fatal(err)
	}
	n, latest, err := d.RetryableSweepRunsSince(ctx, sweepT0())
	if err != nil || n != 3 || !latest.Equal(sweepT0().Add(5*time.Hour)) {
		t.Errorf("RetryableSweepRunsSince = %d, %v, %v; want 3, %v (a crashed run has no finish, so its start)",
			n, latest, err, sweepT0().Add(5*time.Hour))
	}
	n, latest, err = d.RetryableSweepRunsSince(ctx, sweepT0().Add(6*time.Hour))
	if err != nil || n != 0 || !latest.IsZero() {
		t.Errorf("RetryableSweepRunsSince after all runs = %d, %v, %v; want 0, zero", n, latest, err)
	}
}

func TestLiveGrabsSince(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	for _, r := range []struct {
		hours   int
		dry     bool
		results []string
	}{
		{-1, false, []string{"imported"}},
		{1, false, []string{"imported", "no_pack", "failed", "searched"}},
		{2, true, []string{"would_grab", "imported"}},
		{3, false, []string{"refused", "grabbing"}},
	} {
		at := sweepT0().Add(time.Duration(r.hours) * time.Hour)
		id, err := d.CreateSweepRun(ctx, "schedule", r.dry, at)
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range r.results {
			if err = d.AddSweepItem(ctx, &db.SweepItem{RunID: id, Target: "season", SeriesID: 1, SeriesTitle: "Show",
				ReleaseTitle: "Show.S01", Result: res, At: at}); err != nil {
				t.Fatal(err)
			}
		}
		if err = d.AddSweepItem(ctx, &db.SweepItem{RunID: id, Target: "season", SeriesID: 1, SeriesTitle: "Show",
			Episodes: "1-3", Result: "failed", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := d.LiveGrabsSince(ctx, sweepT0()); err != nil || n != 4 {
		t.Errorf("LiveGrabsSince = %d, %v; want 4 (a failed episode search is not a grab)", n, err)
	}
}

func TestLiveSearchesSince(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	for _, r := range []struct {
		hours    int
		dry      bool
		searched int
	}{{-1, false, 7}, {1, false, 10}, {2, true, 30}, {3, false, 5}} {
		at := sweepT0().Add(time.Duration(r.hours) * time.Hour)
		id, err := d.CreateSweepRun(ctx, "schedule", r.dry, at)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.FinishSweepRun(ctx, id, "interrupted", "", db.SweepCounts{Searched: r.searched}, at); err != nil {
			t.Fatal(err)
		}
	}
	searched, crashed, err := d.LiveSearchesSince(ctx, sweepT0())
	if err != nil || searched != 15 || crashed {
		t.Errorf("LiveSearchesSince = %d, %v, %v; want 15, false", searched, crashed, err)
	}
	if _, err = d.CreateSweepRun(ctx, "schedule", false, sweepT0().Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = d.InterruptRunningSweeps(ctx); err != nil {
		t.Fatal(err)
	}
	if _, crashed, err = d.LiveSearchesSince(ctx, sweepT0()); err != nil || !crashed {
		t.Errorf("LiveSearchesSince after a crash: crashed = %v, %v; want true", crashed, err)
	}
}

func TestDB_LiveLookupsSince(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	one := 1
	for _, r := range []struct {
		hours  int
		dry    bool
		lookup []*int
	}{
		{-25, false, []*int{&one}},
		{1, false, []*int{&one, &one, nil}},
		{2, true, []*int{&one, &one}},
	} {
		at := sweepT0().Add(time.Duration(r.hours) * time.Hour)
		id, err := d.CreateSweepRun(ctx, "schedule", r.dry, at)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range r.lookup {
			if err = d.AddSweepItem(ctx, &db.SweepItem{RunID: id, Target: "season", SeriesID: 1, SeriesTitle: "Show",
				Result: "no_pack", LookupResults: l, At: at}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := d.LiveLookupsSince(ctx, sweepT0().Add(-24*time.Hour)); err != nil || n != 2 {
		t.Errorf("LiveLookupsSince = %d, %v; want 2", n, err)
	}
}

func TestDB_LiveSearchesSince_RunningNotCrashed(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0()); err != nil {
		t.Fatal(err)
	}
	if _, crashed, err := d.LiveSearchesSince(ctx, sweepT0()); err != nil || crashed {
		t.Errorf("running run: crashed = %v, %v; want false", crashed, err)
	}
	if _, err := d.InterruptRunningSweeps(ctx); err != nil {
		t.Fatal(err)
	}
	if _, crashed, err := d.LiveSearchesSince(ctx, sweepT0()); err != nil || !crashed {
		t.Errorf("interrupted run: crashed = %v, %v; want true", crashed, err)
	}
}

func TestGetSweepRun(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.GetSweepRun(ctx, 99); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("missing run err = %v, want ErrNotFound", err)
	}
	id, err := d.CreateSweepRun(ctx, "manual", true, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	c := db.SweepCounts{Grabs: 1, Lookups: 2}
	if err = d.FinishSweepRun(ctx, id, "done", "", c, sweepT0().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	r, err := d.GetSweepRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id || r.Trigger != "manual" || !r.Dry || r.Status != "done" || r.Counts.Grabs != 1 ||
		r.Counts.Lookups != 2 || !r.StartedAt.Equal(sweepT0()) || r.FinishedAt == nil ||
		!r.FinishedAt.Equal(sweepT0().Add(time.Minute)) {
		t.Fatalf("run = %+v", r)
	}
}
