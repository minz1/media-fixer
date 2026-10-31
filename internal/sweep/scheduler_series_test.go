package sweep_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func (h *harness) seriesByID(id int) client.Series {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, s := range h.fake.series {
		if s.ID == id {
			return s
		}
	}
	return client.Series{}
}

func twoShows(t *testing.T, s sweep.Settings) (*harness, *sweep.Scheduler) {
	t.Helper()
	h, sch := scheduledAt(t, t0(), s)
	h.fake.addSeries(1, "A-title", missingEps(1, 1, 11, 12)...)
	h.fake.addSeries(2, "B-title", missingEps(2, 1, 21, 22, 23)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "A S01", "Zilean", 10))
	h.fake.setReleases(seasonKey(2, 1), seasonRel("g2", "B S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	h.fake.plan("g2", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	return h, sch
}

func (h *harness) runSeries(t *testing.T, sch *sweep.Scheduler, id int, pass sweep.Pass) {
	t.Helper()
	if err := sch.RunSeries(h.seriesByID(id), pass); err != nil {
		t.Fatal(err)
	}
	sch.Wait()
}

func TestRunSeriesOnlyTouchesSeries(t *testing.T) {
	t.Parallel()
	h, sch := twoShows(t, settings())
	b1 := targetOf("s:2:1", 2, t0().Add(-time.Hour))
	b2 := targetOf("s:2:9", 2, t0().Add(-time.Hour))
	b2.Season = 9
	h.putTarget(t, b1)
	h.putTarget(t, b2)
	resolved := targetOf("s:1:3", 1, t0().Add(-time.Hour))
	resolved.Season = 3
	h.putTarget(t, resolved)
	h.runSeries(t, sch, 1, sweep.PassPacks)
	if h.fake.lookupCount(seasonKey(1, 1)) != 1 || h.fake.lookupCount(seasonKey(2, 1)) != 0 {
		t.Errorf("lookups A=%d B=%d", h.fake.lookupCount(seasonKey(1, 1)), h.fake.lookupCount(seasonKey(2, 1)))
	}
	got := h.targets(t)
	if _, ok := got["s:1:3"]; ok {
		t.Error("A's resolved season row survived")
	}
	for key, want := range map[string]db.SweepTarget{"s:2:1": b1, "s:2:9": b2} {
		if g, ok := got[key]; !ok || g.Attempts != want.Attempts || !g.NextEligible.Equal(want.NextEligible) {
			t.Errorf("%s = %+v, want unchanged %+v", key, g, want)
		}
	}
}

func TestRunSeriesIgnoresNextEligible(t *testing.T) {
	t.Parallel()
	h, sch := twoShows(t, settings())
	a := targetOf("s:1:1", 1, t0().Add(10*24*time.Hour))
	a.Streak = 1
	b := targetOf("s:2:1", 2, t0().Add(10*24*time.Hour))
	h.putTarget(t, a)
	h.putTarget(t, b)
	h.runSeries(t, sch, 1, sweep.PassPacks)
	got := h.targets(t)
	if h.fake.lookupCount(seasonKey(1, 1)) != 1 || h.fake.lookupCount(seasonKey(2, 1)) != 0 {
		t.Errorf("lookups A=%d B=%d", h.fake.lookupCount(seasonKey(1, 1)), h.fake.lookupCount(seasonKey(2, 1)))
	}
	if got["s:1:1"].Streak != 2 {
		t.Errorf("A streak = %d, want 2", got["s:1:1"].Streak)
	}
	if got["s:2:1"].Attempts != b.Attempts || !got["s:2:1"].NextEligible.Equal(b.NextEligible) {
		t.Errorf("B changed: %+v", got["s:2:1"])
	}
}

func TestRunSeriesPacksNoEpisodeSearch(t *testing.T) {
	t.Parallel()
	h, sch := twoShows(t, searchSettings())
	h.fake.setReleases(seasonKey(1, 1))
	h.runSeries(t, sch, 1, sweep.PassPacks)
	if h.fake.lookupCount(seasonKey(1, 1)) != 1 || len(h.fake.commands()) != 0 {
		t.Errorf("lookups %d, commands %v", h.fake.lookupCount(seasonKey(1, 1)), h.fake.commands())
	}
}

func TestRunSeriesSinglesNoLookupOrGrab(t *testing.T) {
	t.Parallel()
	h, sch := twoShows(t, searchSettings())
	h.runSeries(t, sch, 1, sweep.PassSingles)
	if h.fake.lookupCount(seasonKey(1, 1)) != 0 || h.fake.countWrites("POST /release") != 0 {
		t.Error("singles run looked up or grabbed")
	}
	if got := h.fake.commands(); len(got) != 1 || !slices.Equal(got[0], []int{11, 12}) {
		t.Errorf("commands = %v, want A's two episodes", got)
	}
}

func TestRunSeriesSinglesSearchesBurstSeason(t *testing.T) {
	t.Parallel()
	h, sch := twoShows(t, searchSettings())
	h.failures(1, 5)
	h.runSeries(t, sch, 1, sweep.PassSingles)
	if got := h.fake.commands(); len(got) != 1 || !slices.Equal(got[0], []int{11, 12}) {
		t.Errorf("commands = %v, want the burst season searched", got)
	}
}

func TestRunSeriesRemaining(t *testing.T) {
	t.Parallel()
	s := settings()
	s.MaxGrabs, s.MaxSearches = 1, 2
	h, sch := twoShows(t, s)
	h.fake.setReleases(seasonKey(2, 1))
	h.run(t, false, s)
	if used := len(h.fake.commands()); used == 0 {
		t.Fatal("setup: first run searched nothing")
	}
	if err := sch.RunSeries(h.seriesByID(1), sweep.PassPacks); !errors.Is(err, sweep.ErrNoBudget) {
		t.Errorf("packs = %v, want ErrNoBudget", err)
	}
	if err := sch.RunSeries(h.seriesByID(2), sweep.PassSingles); !errors.Is(err, sweep.ErrNoBudget) {
		t.Errorf("singles = %v, want ErrNoBudget", err)
	}
	s.MaxSearches = 3
	if err := sweep.SaveSettings(context.Background(), h.db, s, t0()); err != nil {
		t.Fatal(err)
	}
	h.runSeries(t, sch, 2, sweep.PassSingles)
	cmds := h.fake.commands()
	if last := cmds[len(cmds)-1]; len(cmds) != 2 || len(last) != 1 {
		t.Errorf("commands = %v, want the second capped to one episode", cmds)
	}
}

func TestRunSeriesRefused(t *testing.T) {
	t.Parallel()
	anime := func(sr *client.Series, s *sweep.Settings) { sr.SeriesType, s.Anime = "anime", false }
	for _, tc := range []struct {
		name  string
		edit  func(*client.Series, *sweep.Settings)
		pause bool
		want  error
	}{
		{"disabled", func(_ *client.Series, s *sweep.Settings) { s.Enabled = false }, false, sweep.ErrDisabled},
		{"excluded", func(*client.Series, *sweep.Settings) {}, false, sweep.ErrExcluded},
		{"unmonitored", func(sr *client.Series, _ *sweep.Settings) { sr.Monitored = false }, false, sweep.ErrUnmonitored},
		{"anime off", anime, false, sweep.ErrAnimeOff},
		{"paused", func(*client.Series, *sweep.Settings) {}, true, sweep.ErrPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := settings()
			h, sch := twoShows(t, s)
			sr := h.seriesByID(1)
			tc.edit(&sr, &s)
			if err := sweep.SaveSettings(context.Background(), h.db, s, t0()); err != nil {
				t.Fatal(err)
			}
			if tc.name == "excluded" {
				if err := sweep.SetExcluded(context.Background(), h.db, 1, true); err != nil {
					t.Fatal(err)
				}
			}
			h.setPaused(t, tc.pause)
			if err := sch.RunSeries(sr, sweep.PassPacks); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if n := len(h.runs(t)); n != 0 {
				t.Errorf("%d runs created", n)
			}
		})
	}
}

func TestRunSeriesRefusedSettingsAndRun(t *testing.T) {
	t.Parallel()
	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()
		h, sch := twoShows(t, settings())
		if err := h.db.SetSetting(context.Background(), sweep.SettingsKey, "{"); err != nil {
			t.Fatal(err)
		}
		if err := sch.RunSeries(h.seriesByID(1), sweep.PassPacks); !errors.Is(err, sweep.ErrCorruptSettings) {
			t.Errorf("err = %v, want ErrCorruptSettings", err)
		}
		if n := len(h.runs(t)); n != 0 {
			t.Errorf("%d runs created", n)
		}
	})
	t.Run("active", func(t *testing.T) {
		t.Parallel()
		h, sch := twoShows(t, settings())
		entered, release := make(chan struct{}), make(chan struct{})
		h.fake.beforePost = func() {
			close(entered)
			<-release
		}
		if err := sch.RunNow("manual", false); err != nil {
			t.Fatal(err)
		}
		<-entered
		if err := sch.RunSeries(h.seriesByID(1), sweep.PassPacks); !errors.Is(err, sweep.ErrRunActive) {
			t.Errorf("err = %v, want ErrRunActive", err)
		}
		close(release)
		sch.Wait()
		if n := len(h.runs(t)); n != 1 {
			t.Errorf("%d runs, want only the manual one", n)
		}
	})
}

func TestRunSeriesTriggerAndNoDigest(t *testing.T) {
	t.Parallel()
	h, sch := twoShows(t, settings())
	h.seedStuck(t, "s:2:1", 1)
	before := sch.NextRun(t0())
	h.runSeries(t, sch, 1, sweep.PassPacks)
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Trigger != "babysit: A-title" {
		t.Fatalf("runs = %+v", runs)
	}
	if d := h.digests(); len(d) != 0 {
		t.Errorf("digest sent: %v", d)
	}
	if after := sch.NextRun(t0()); before == nil || after == nil || !before.Equal(*after) {
		t.Errorf("next run %v -> %v", before, after)
	}
}
