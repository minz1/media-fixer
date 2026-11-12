package sweep_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func TestS3_7_GrabOutcomeMovesFromGrabbing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Survivor", missingEps(1, 22, 1, 2, 3)...)
	h.fake.setReleases(seasonKey(1, 22), loadReleases(t, "rel-sv-22.json.gz")...)
	var seen []sweepstate.ItemResult
	h.fake.beforePost = func() {
		runs, err := h.db.ListSweepRuns(context.Background(), 1)
		if err != nil || len(runs) == 0 {
			t.Errorf("list runs: %v", err)
			return
		}
		for _, it := range runs[0].Items {
			seen = append(seen, it.Result)
		}
	}
	run := h.run(t, false, settings())
	if run.Status != sweepstate.Done {
		t.Fatalf("status %s (%s), want done", run.Status, run.Reason)
	}
	if !slices.Equal(seen, []sweepstate.ItemResult{sweepstate.Grabbing}) {
		t.Errorf("items at POST time = %v, want [grabbing]", seen)
	}
	if len(run.Items) != 1 || run.Items[0].Result != sweepstate.Downloading {
		t.Errorf("items = %+v, want one downloading item", run.Items)
	}
}

func TestS3_12_SearchFailureMovesFromSearched(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.commandStatus = 400
	run := h.run(t, false, searchSettings())
	got := itemResults(run)
	if !slices.Equal(got, []string{"no_pack", "failed"}) {
		t.Fatalf("results = %v, want [no_pack failed]", got)
	}
	if run.Items[1].Episodes == "" || run.Items[1].ReleaseTitle != "" {
		t.Errorf("failed search item = %+v, want episodes and no release", run.Items[1])
	}
}

func TestS3_9_GrabbingSurvivesInterrupt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	armed := false
	h.fake.beforePost = func() { armed = true }
	sleep := h.runner.Sleep
	h.runner.Sleep = func(c context.Context, d time.Duration) error {
		if armed {
			cancel()
		}
		return sleep(c, d)
	}
	id, err := h.runner.Run(ctx, "manual", false, settings())
	if err == nil {
		t.Fatal("Run after cancel returned no error")
	}
	run, err := h.db.GetSweepRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := h.db.ListSweepRuns(context.Background(), 1)
	if err != nil || len(stored) != 1 {
		t.Fatalf("list runs: %v", err)
	}
	if run.Status != sweepstate.Interrupted || !slices.Equal(itemResults(stored[0]), []string{"grabbing"}) {
		t.Errorf("status %s items %v, want interrupted and still grabbing", run.Status, itemResults(stored[0]))
	}
}

func TestS3_AddItemRefusesIllegalInsert(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	cases := []struct {
		name string
		dry  bool
		item db.SweepItem
	}{
		{"would_grab in a live run", false, db.SweepItem{Result: sweepstate.WouldGrab, ReleaseTitle: "Show.S01"}},
		{"grabbing with no release", false, db.SweepItem{Result: sweepstate.Grabbing}},
		{"grabbing in a dry run", true, db.SweepItem{Result: sweepstate.Grabbing, ReleaseTitle: "Show.S01"}},
		{"imported is not initial", false, db.SweepItem{Result: sweepstate.Imported, ReleaseTitle: "Show.S01"}},
	}
	for _, tc := range cases {
		id, err := h.db.CreateSweepRun(ctx, "manual", tc.dry, t0())
		if err != nil {
			t.Fatal(err)
		}
		it := tc.item
		it.RunID, it.Target, it.SeriesID, it.SeriesTitle = id, "s:1:1", 1, "Show"
		if err = sweep.AddItemForTest(ctx, h.runner, tc.dry, &it); err == nil {
			t.Errorf("%s: insert accepted", tc.name)
		}
		runs, lerr := h.db.ListSweepRuns(ctx, 1)
		if lerr != nil || len(runs) != 1 || len(runs[0].Items) != 0 {
			t.Errorf("%s: rows after the refused insert = %+v, %v", tc.name, runs, lerr)
		}
	}
}

func TestE1_SonarrWakesDoNotShortenTheGrabWait(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 20})
	sleep := h.runner.Sleep
	h.runner.Sleep = func(c context.Context, d time.Duration) error {
		if d != 30*time.Second {
			return sleep(c, d)
		}
		h.runner.WakeDownload()
		select {
		case <-c.Done():
			return c.Err()
		case <-time.After(5 * time.Second):
			return sleep(c, d)
		}
	}
	run := h.run(t, false, settings())
	if len(run.Items) != 1 || run.Items[0].Result != sweepstate.Imported {
		t.Errorf("items = %+v, want one imported item after woken polls", run.Items)
	}
}
