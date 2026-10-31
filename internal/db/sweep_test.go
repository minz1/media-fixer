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

func TestDB_LiveBudgetsCountSpendInWindow(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	now := sweepT0()
	one := 1
	id, err := d.CreateSweepRun(ctx, "schedule", false, now.Add(-25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []db.SweepItem{
		{ReleaseTitle: "Old.S01", Result: "imported", LookupResults: &one, At: now.Add(-24*time.Hour - time.Minute)},
		{ReleaseTitle: "New.S01", Result: "imported", LookupResults: &one, At: now.Add(-23 * time.Hour)},
		{Result: "no_pack", LookupResults: &one, At: now.Add(-23 * time.Hour)},
	} {
		it.RunID, it.Target, it.SeriesID, it.SeriesTitle = id, "season", 1, "Show"
		if err = d.AddSweepItem(ctx, &it); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.FinishSweepRun(ctx, id, "done", "", db.SweepCounts{Searched: 4}, now.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	since := now.Add(-24 * time.Hour)
	grabs, gerr := d.LiveGrabsSince(ctx, since)
	if gerr != nil || grabs != 1 {
		t.Errorf("LiveGrabsSince = %d, %v; want 1 (the grab 23 h ago in a run started 25 h ago)", grabs, gerr)
	}
	lookups, lerr := d.LiveLookupsSince(ctx, since)
	if lerr != nil || lookups != 2 {
		t.Errorf("LiveLookupsSince = %d, %v; want 2", lookups, lerr)
	}
	searched, _, serr := d.LiveSearchesSince(ctx, since)
	if serr != nil || searched != 4 {
		t.Errorf("LiveSearchesSince = %d, %v; want 4 (the run finished 23 h ago)", searched, serr)
	}
}

func TestLastSweepRunStartIgnoresBabysit(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateSweepRun(ctx, db.BabysitTrigger+" Foo", false, sweepT0().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := d.LastSweepRunStart(ctx, nil)
	if err != nil || got == nil || !got.Equal(sweepT0()) {
		t.Errorf("LastSweepRunStart = %v, %v; want %v", got, err, sweepT0())
	}
}

func TestRetryableSweepRunsSinceIgnoresBabysit(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id, err := d.CreateSweepRun(ctx, db.BabysitTrigger+" Foo", false, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishSweepRun(ctx, id, "failed", "", db.SweepCounts{}, sweepT0().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	n, _, err := d.RetryableSweepRunsSince(ctx, sweepT0().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Errorf("RetryableSweepRunsSince = %d, %v; want 0", n, err)
	}
}

func addItem(t *testing.T, d *db.DB, runID int64, seriesID int, result string, at time.Time) {
	t.Helper()
	it := db.SweepItem{RunID: runID, Target: "t", SeriesID: seriesID, SeriesTitle: "S", Result: result, At: at}
	if err := d.AddSweepItem(context.Background(), &it); err != nil {
		t.Fatal(err)
	}
}

func TestSeriesSweepItemsLiveOnly(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	live, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	dry, err := d.CreateSweepRun(ctx, "manual", true, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	addItem(t, d, live, 1, "old", sweepT0())
	addItem(t, d, live, 1, "mid", sweepT0().Add(time.Hour))
	addItem(t, d, live, 1, "new", sweepT0().Add(2*time.Hour))
	addItem(t, d, dry, 1, "dry", sweepT0().Add(3*time.Hour))
	addItem(t, d, live, 2, "other", sweepT0().Add(4*time.Hour))

	got, err := d.SeriesSweepItems(ctx, 1, 10)
	if err != nil || len(got) != 3 || got[0].Result != "new" || got[1].Result != "mid" || got[2].Result != "old" {
		t.Fatalf("SeriesSweepItems = %+v, %v", got, err)
	}
	got, err = d.SeriesSweepItems(ctx, 1, 2)
	if err != nil || len(got) != 2 || got[0].Result != "new" {
		t.Errorf("limited = %+v, %v", got, err)
	}
}

func TestLatestSweepItems(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	live, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	dry, err := d.CreateSweepRun(ctx, "manual", true, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	addItem(t, d, live, 1, "a-old", sweepT0())
	addItem(t, d, live, 1, "a-new", sweepT0().Add(time.Hour))
	addItem(t, d, dry, 1, "a-dry", sweepT0().Add(2*time.Hour))
	addItem(t, d, live, 2, "b-tie", sweepT0())
	addItem(t, d, live, 2, "b-tie-later-id", sweepT0())
	addItem(t, d, dry, 2, "b-dry", sweepT0().Add(time.Hour))

	got, err := d.LatestSweepItems(ctx)
	if err != nil || len(got) != 2 || got[1].Result != "a-new" || got[2].Result != "b-tie-later-id" {
		t.Fatalf("LatestSweepItems = %+v, %v", got, err)
	}
}
