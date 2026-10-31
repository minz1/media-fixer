package sweep_test

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func (h *harness) failures(season, n int) {
	for i := range n {
		h.fake.addFailure(1, 900+i, season, "DL"+strconv.Itoa(i), t0().Add(-time.Hour))
	}
}

func backedOff(key string, season int) db.SweepTarget {
	tg := targetOf(key, 1, t0().Add(10*24*time.Hour))
	tg.Season, tg.Streak = season, 2
	return tg
}

func TestRun_BurstClearsBackoffAndGoesFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeriesAdded(2, "New", t0().Add(-24*time.Hour), missingEps(2, 1, 21, 22)...)
	h.fake.addSeriesAdded(1, "Old", t0().Add(-400*24*time.Hour), missingEps(1, 1, 11, 12)...)
	h.putTarget(t, backedOff("s:1:1", 1))
	h.failures(1, 5)
	run := h.run(t, false, settings())
	if len(run.Items) != 2 || run.Items[0].Target != "s:1:1" || run.Items[1].Target != "s:2:1" {
		t.Fatalf("items = %+v", run.Items)
	}
	tg := h.targets(t)["s:1:1"]
	if tg.Streak != 3 || tg.Attempts != 2 || !tg.NextEligible.Equal(t0().Add(8*24*time.Hour)) {
		t.Errorf("row after burst = %+v", tg)
	}
}

func TestRun_BurstReplay(t *testing.T) {
	t.Parallel()
	t.Run("standard", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fake.addSeries(1, "Survivor", missingEps(1, 22, 1, 2, 3)...)
		h.fake.setReleases(seasonKey(1, 22), loadReleases(t, "rel-sv-22.json.gz")...)
		h.putTarget(t, backedOff("s:1:22", 22))
		h.failures(22, 5)
		h.run(t, false, settings())
		if got := h.fake.countWrites("POST /release"); got != 1 {
			t.Errorf("POSTs = %d, want the season pack grabbed", got)
		}
	})
	t.Run("anime", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		var ids []int
		for id := 6402; id <= 6427; id++ {
			ids = append(ids, id)
		}
		h.fake.addAnime(1, "Adventure Time", missingEps(1, 3, ids...)...)
		h.fake.setReleases("e6402", loadReleases(t, "rel-at-s3.json.gz")...)
		h.putTarget(t, backedOff("s:1:3", 3))
		h.failures(3, 5)
		h.run(t, false, settings())
		if got := h.fake.countWrites("POST /release"); got != 1 {
			t.Errorf("POSTs = %d, want the season pack grabbed", got)
		}
	})
	t.Run("no burst stays backed off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.fake.addSeries(1, "Survivor", missingEps(1, 22, 1, 2, 3)...)
		h.fake.setReleases(seasonKey(1, 22), loadReleases(t, "rel-sv-22.json.gz")...)
		h.putTarget(t, backedOff("s:1:22", 22))
		h.failures(22, 4)
		h.run(t, false, settings())
		if got := h.fake.countWrites("POST /release"); got != 0 {
			t.Errorf("POSTs = %d, want none", got)
		}
	})
}

func TestRun_BurstWaitsForGrabBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeriesAdded(2, "New", t0().Add(-24*time.Hour), missingEps(2, 1, 21, 22)...)
	h.fake.addSeriesAdded(1, "Old", t0().Add(-400*24*time.Hour), missingEps(1, 1, 11, 12)...)
	h.putTarget(t, backedOff("s:1:1", 1))
	h.failures(1, 5)
	spent := searchSettings()
	spent.MaxGrabs = 0
	h.run(t, false, spent)
	if h.fake.lookupCount(seasonKey(1, 1)) != 0 {
		t.Errorf("burst season looked up with no grab budget")
	}
	if got := h.fake.commands(); !slices.EqualFunc(got, [][]int{{21, 22}}, slices.Equal) {
		t.Errorf("commands = %v, want only the non-burst season", got)
	}
	if tg := h.targets(t)["s:1:1"]; tg.Attempts != 1 || !tg.LastAttempt.Equal(t0().Add(-48*time.Hour)) {
		t.Errorf("burst row changed: %+v", tg)
	}
	h.setClock(t0().Add(time.Hour))
	run := h.run(t, false, settings())
	if len(run.Items) == 0 || run.Items[0].Target != "s:1:1" {
		t.Errorf("next run items = %+v, want the burst season first", run.Items)
	}
}

func TestRun_FailureHistoryErrorContinues(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.runner.Log = slog.New(slog.NewTextHandler(logs, nil))
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.historyStatus = 500
	run := h.run(t, false, settings())
	if run.Status != "done" || h.fake.lookupCount(seasonKey(1, 1)) != 1 {
		t.Errorf("status %s, lookups %d", run.Status, h.fake.lookupCount(seasonKey(1, 1)))
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if !strings.Contains(logs.buf.String(), "level=WARN msg=\"sweep failure history unavailable\"") {
		t.Errorf("logs = %s", logs.buf.String())
	}
}

func TestRun_FailureHistoryCancelled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	ctx, cancel := context.WithCancel(context.Background())
	h.fake.beforeHistory = cancel
	id, _ := h.runner.Run(ctx, "manual", false, settings())
	runs, err := h.db.ListSweepRuns(context.Background(), 10)
	if err != nil || len(runs) != 1 || runs[0].ID != id {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
	if runs[0].Status != "interrupted" || h.fake.lookupCount(seasonKey(1, 1)) != 0 {
		t.Errorf("status %s, lookups %d", runs[0].Status, h.fake.lookupCount(seasonKey(1, 1)))
	}
}

func TestRun_BurstBelowMinMissingLookedUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Old", missingEps(1, 1, 11)...)
	h.failures(1, 5)
	s := settings()
	s.MinMissing, s.MaxSearches = 2, 10
	run := h.run(t, false, s)
	if got := itemResults(run); !slices.Equal(got, []string{"no_pack", "searched"}) {
		t.Errorf("results = %v, want a pack lookup then the episode searched", got)
	}
}
