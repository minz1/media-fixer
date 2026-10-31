package sweep_test

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

func (h *harness) targets(t *testing.T) map[string]db.SweepTarget {
	t.Helper()
	got, err := h.db.ListSweepTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (h *harness) putTarget(t *testing.T, tg db.SweepTarget) {
	t.Helper()
	if err := h.db.UpsertSweepTarget(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) setClock(at time.Time) {
	h.clock.mu.Lock()
	h.clock.t = at
	h.clock.mu.Unlock()
}

func (h *harness) setHasFile(episodeID int) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for i := range h.fake.episodes[1] {
		if h.fake.episodes[1][i].ID == episodeID {
			h.fake.episodes[1][i].HasFile = true
		}
	}
}

func targetOf(key string, seriesID int, next time.Time) db.SweepTarget {
	return db.SweepTarget{
		Target: key, SeriesID: seriesID, SeriesTitle: "Show", Season: 1, Missing: 2, Attempts: 1,
		LastAttempt: t0().Add(-48 * time.Hour), NextEligible: next,
	}
}

func TestRun_OneAttemptBothPasses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	run := h.run(t, false, searchSettings())
	if got := itemResults(run); !slices.Equal(got, []string{"no_pack", "searched"}) {
		t.Fatalf("results = %v", got)
	}
	got := h.targets(t)
	tg, ok := got["s:1:1"]
	if len(got) != 1 || !ok || tg.Attempts != 1 || tg.Streak != 0 || tg.Missing != 2 {
		t.Errorf("targets = %+v", got)
	}
}

func TestRun_StreakGrowsAndResets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12, 13)...)
	h.run(t, false, settings())
	first := h.targets(t)["s:1:1"]
	if first.Streak != 0 || !first.NextEligible.Equal(t0().Add(24*time.Hour)) {
		t.Fatalf("run 1 row = %+v", first)
	}
	h.setClock(first.NextEligible)
	h.run(t, false, settings())
	second := h.targets(t)["s:1:1"]
	if second.Streak != 1 || second.Attempts != 2 || !second.NextEligible.Equal(first.NextEligible.Add(48*time.Hour)) {
		t.Fatalf("run 2 row = %+v", second)
	}
	h.setHasFile(11)
	h.setClock(second.NextEligible)
	h.run(t, false, settings())
	third := h.targets(t)["s:1:1"]
	if third.Streak != 0 || third.Missing != 2 || third.Attempts != 3 {
		t.Errorf("run 3 row = %+v", third)
	}
}

func TestRun_IneligibleSkippedByBothPasses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", append(missingEps(1, 1, 11, 12), missingEps(1, 2, 21, 22)...)...)
	h.putTarget(t, targetOf("s:1:1", 1, t0().Add(time.Hour)))
	h.run(t, false, searchSettings())
	if h.fake.lookupCount(seasonKey(1, 1)) != 0 || h.fake.lookupCount(seasonKey(1, 2)) != 1 {
		t.Errorf("lookups s1 %d, s2 %d", h.fake.lookupCount(seasonKey(1, 1)), h.fake.lookupCount(seasonKey(1, 2)))
	}
	if got := h.fake.commands(); !slices.EqualFunc(got, [][]int{{21, 22}}, slices.Equal) {
		t.Errorf("commands = %v, want only season 2", got)
	}
	if tg := h.targets(t)["s:1:1"]; tg.Attempts != 1 {
		t.Errorf("ineligible row = %+v", tg)
	}
}

func TestRun_AnimeChunkKeepsSeasonRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addAnime(1, "Anime", missingEps(1, 1, 1, 2, 3, 4)...)
	h.run(t, false, searchSettings())
	first := h.targets(t)
	if len(first) != 1 || first["s:1:1"].Missing != 4 || first["s:1:1"].Streak != 0 {
		t.Fatalf("run 1 targets = %+v", first)
	}
	h.setHasFile(1)
	h.setClock(first["s:1:1"].NextEligible)
	h.run(t, false, searchSettings())
	second := h.targets(t)
	row := second["s:1:1"]
	if len(second) != 1 || row.Missing != 3 || row.Streak != 0 || row.Attempts != 2 {
		t.Fatalf("run 2 targets = %+v", second)
	}
	h.setClock(second["s:1:1"].NextEligible)
	h.run(t, false, searchSettings())
	if tg := h.targets(t)["s:1:1"]; tg.Streak != 1 || tg.Attempts != 3 {
		t.Errorf("run 3 row = %+v", tg)
	}
}

func TestRun_AnimeChunkSpansSeasons(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addAnime(1, "Anime", append(missingEps(1, 1, 1, 2), missingEps(1, 2, 3, 4, 5)...)...)
	s := searchSettings()
	s.MaxLookups = 0
	h.run(t, false, s)
	got := h.targets(t)
	if len(got) != 2 || got["s:1:1"].Attempts != 1 || got["s:1:1"].Missing != 2 ||
		got["s:1:2"].Attempts != 1 || got["s:1:2"].Missing != 3 {
		t.Errorf("targets = %+v", got)
	}
}

func TestRun_DryRunWritesNoTargets(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	row := targetOf("s:1:1", 1, t0().Add(-time.Hour))
	stale := targetOf("s:9:1", 9, t0().Add(-time.Hour))
	h.putTarget(t, row)
	h.putTarget(t, stale)
	h.run(t, true, searchSettings())
	got := h.targets(t)
	if len(got) != 2 || got["s:1:1"] != row || got["s:9:1"] != stale {
		t.Errorf("targets after dry run = %+v", got)
	}
}

func TestRun_OrderNewestSeriesFewestMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeriesAdded(1, "A", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		append(missingEps(1, 1, 11, 12, 13, 14, 15), missingEps(1, 2, 21, 22)...)...)
	h.fake.addSeriesAdded(2, "B", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), missingEps(2, 1, 31, 32, 33)...)
	run := h.run(t, false, searchSettings())
	var packs, searches []string
	for _, it := range run.Items {
		switch it.Result {
		case "no_pack":
			packs = append(packs, it.Target)
		case "searched":
			searches = append(searches, it.Target)
		}
	}
	want := []string{"s:2:1", "s:1:2", "s:1:1"}
	if !slices.Equal(packs, want) || !slices.Equal(searches, want) {
		t.Errorf("lookups %v, searches %v, want %v", packs, searches, want)
	}
	wantCmds := [][]int{{31, 32, 33}, {21, 22}, {11, 12, 13, 14, 15}}
	if got := h.fake.commands(); !slices.EqualFunc(got, wantCmds, slices.Equal) {
		t.Errorf("commands = %v", got)
	}
}

func TestRun_ResolvedTargetsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	added := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
	done := missingEps(1, 1, 11, 12)
	h.fake.addSeries(1, "Done", done...)
	h.setHasFile(11)
	h.setHasFile(12)
	h.fake.addSeries(2, "Excluded", missingEps(2, 1, 21, 22)...)
	h.fake.addAnime(3, "Unreached", missingEps(3, 1, 31, 32)...)
	h.fake.addSeriesAdded(4, "Capped", added(2024), append(missingEps(4, 1, 41, 42), missingEps(4, 2, 43, 44)...)...)
	h.fake.addSeries(5, "Queued", missingEps(5, 1, 51, 52)...)
	h.fake.queue = []client.QueueDetail{{ID: 1, EpisodeID: 51}, {ID: 2, EpisodeID: 52}}
	h.fake.addSeriesAdded(6, "Grabbed", added(2025), missingEps(6, 1, 61, 62)...)
	h.fake.setReleases(seasonKey(6, 1), seasonRel("g6", "Grabbed S01", "Zilean", 10))
	h.fake.plan("g6", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	past := t0().Add(-time.Hour)
	for _, tg := range []db.SweepTarget{
		targetOf("s:1:1", 1, past), targetOf("s:2:1", 2, past), targetOf("s:3:1", 3, past),
		targetOf("s:5:1", 5, past), targetOf("s:6:1", 6, past),
	} {
		tg.Streak = 2
		h.putTarget(t, tg)
	}
	s := settings()
	s.MaxLookups = 2
	s.ExcludedSeries = []int{2}
	h.run(t, false, s)
	got := h.targets(t)
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, []string{"s:3:1", "s:4:1", "s:5:1", "s:6:1"}) {
		t.Fatalf("targets = %v", keys)
	}
	if got["s:5:1"].Streak != 2 || got["s:5:1"].Attempts != 1 {
		t.Errorf("queued row changed: %+v", got["s:5:1"])
	}
	if got["s:6:1"].Attempts != 2 || got["s:6:1"].Streak != 3 {
		t.Errorf("grabbed row = %+v, want written this run", got["s:6:1"])
	}
}

func TestRun_OldNoPackMemoryIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	yesterday := t0().Add(-24 * time.Hour)
	old, err := h.db.CreateSweepRun(ctx, "schedule", false, yesterday)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.FinishSweepRun(ctx, old, "done", "", db.SweepCounts{}, yesterday); err != nil {
		t.Fatal(err)
	}
	season := 1
	item := db.SweepItem{RunID: old, Target: "s:1:1", SeriesID: 1, SeriesTitle: "Show", Season: &season,
		Result: "no_pack", At: yesterday}
	if err = h.db.AddSweepItem(ctx, &item); err != nil {
		t.Fatal(err)
	}
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	run := h.run(t, false, settings())
	if got := h.fake.lookupCount(seasonKey(1, 1)); got != 1 {
		t.Errorf("lookups = %d, want 1 (yesterday's no_pack is not memory any more)", got)
	}
	if got := itemResults(run); !slices.Equal(got, []string{"no_pack"}) {
		t.Errorf("results = %v", got)
	}
}
