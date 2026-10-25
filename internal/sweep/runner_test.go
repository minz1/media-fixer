package sweep_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func settings() sweep.Settings {
	s := sweep.DefaultSettings()
	s.Enabled = true
	return s
}

func TestSettings_DefaultsAndValidate(t *testing.T) {
	t.Parallel()
	s := sweep.DefaultSettings()
	want := sweep.Settings{
		Weekday: 0, HourUTC: 12, MinMissing: 1, MaxGrabs: 5, MaxLookups: 60, MaxSearches: 0,
		Anime: true, ExcludedSeries: []int{},
	}
	if s.Enabled || s.Weekday != want.Weekday || s.HourUTC != want.HourUTC || s.MinMissing != want.MinMissing ||
		s.MaxGrabs != want.MaxGrabs || s.MaxLookups != want.MaxLookups || s.MaxSearches != want.MaxSearches ||
		!s.Anime || s.ExcludedSeries == nil || len(s.ExcludedSeries) != 0 {
		t.Errorf("defaults = %+v", s)
	}
	if errs := s.Validate(); len(errs) != 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
	bad := s
	bad.MaxGrabs = 0
	bad.HourUTC = 24
	errs := bad.Validate()
	if errs["max_grabs"] == "" || errs["hour_utc"] == "" || len(errs) != 2 {
		t.Errorf("Validate = %v, want max_grabs and hour_utc", errs)
	}
}

func TestSettings_LoadSaveRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	got, err := sweep.LoadSettings(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxGrabs != 5 {
		t.Errorf("absent settings MaxGrabs = %d, want default 5", got.MaxGrabs)
	}
	got.MaxGrabs = 2
	got.ExcludedSeries = []int{7}
	if err = sweep.SaveSettings(ctx, h.db, got, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	raw, _ := h.db.GetSetting(ctx, sweep.SettingsKey)
	if !strings.Contains(raw, `"max_grabs":2`) || !strings.Contains(raw, `"excluded_series":[7]`) {
		t.Errorf("stored = %s", raw)
	}
	again, err := sweep.LoadSettings(ctx, h.db)
	if err != nil || again.MaxGrabs != 2 || !slices.Equal(again.ExcludedSeries, []int{7}) {
		t.Errorf("reload = %+v, %v", again, err)
	}
}

func TestRun_RefusedWhenRedownloadOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.flags.AutoRedownloadFailed = true
	h.fake.flags.AutoRedownloadFailedFromInteractiveSearch = true
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	run := h.run(t, false, settings())
	if run.Status != "refused" || run.Reason != "autoRedownloadFailedFromInteractiveSearch is on" {
		t.Errorf("status = %s (%s)", run.Status, run.Reason)
	}
	if h.fake.lookupCount(seasonKey(1, 1)) != 0 || len(h.fake.writeLog()) != 0 {
		t.Errorf("refused run touched sonarr: lookups %d, writes %v",
			h.fake.lookupCount(seasonKey(1, 1)), h.fake.writeLog())
	}
	if len(*h.dms) != 1 {
		t.Errorf("dms = %v, want one summary", *h.dms)
	}
}

func TestRun_PausedAtPreflight(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.setPaused(t, true)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	run := h.run(t, false, settings())
	if run.Status != "paused" {
		t.Errorf("status = %s", run.Status)
	}
	if h.fake.lookupCount(seasonKey(1, 1)) != 0 {
		t.Error("paused run looked up releases")
	}
}

func TestRun_SeasonImportedFirstTry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 2, 21, 22, 23)...)
	h.fake.setReleases(seasonKey(1, 2), seasonRel("g1", "Show S02 1080p", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 2})
	run := h.run(t, false, settings())
	if run.Status != "done" {
		t.Fatalf("status = %s (%s)", run.Status, run.Reason)
	}
	if got := h.fake.countWrites("POST /release"); got != 1 {
		t.Errorf("POSTs = %d, want 1", got)
	}
	if len(run.Items) != 1 || run.Items[0].Result != "imported" || run.Items[0].Target != "s:1:2" ||
		!strings.EqualFold(run.Items[0].DownloadID, "dl-g1") {
		t.Errorf("items = %+v", run.Items)
	}
	if run.Items[0].LookupResults == nil || *run.Items[0].LookupResults != 1 {
		t.Errorf("lookup_results = %v, want 1", run.Items[0].LookupResults)
	}
	if run.Counts.Imported != 1 || run.Counts.Grabs != 1 || run.Counts.Lookups != 1 || run.Counts.MaxGrabs != 5 {
		t.Errorf("counts = %+v", run.Counts)
	}
	if len(*h.dms) != 1 || !strings.Contains((*h.dms)[0], "/media/maintenance") {
		t.Errorf("dms = %v", *h.dms)
	}
	if _, active := h.runner.Progress(); active {
		t.Error("Progress reports active after the run")
	}
}

func TestRun_FailedThenNextCandidate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1),
		seasonRel("g1", "Show S01 A", "Zilean", 10),
		seasonRel("g2", "Show S01 B", "Zilean", 20))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFailed", after: 1})
	h.fake.plan("g2", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	run := h.run(t, false, settings())
	if got := itemResults(run); !slices.Equal(got, []string{"failed", "imported"}) {
		t.Errorf("results = %v", got)
	}
	if !slices.Equal(h.fake.writeLog(), []string{"POST /release g1", "POST /release g2"}) {
		t.Errorf("writes = %v", h.fake.writeLog())
	}
	if run.Counts.Failed != 1 || run.Counts.Imported != 1 {
		t.Errorf("counts = %+v", run.Counts)
	}
}

func TestRun_Post500NoGrab_Refused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1),
		seasonRel("g1", "Show.S01.DDP.2.0", "Comet", 10),
		seasonRel("g2", "Show S01 B", "Comet", 20))
	h.fake.plan("g1", grabPlan{status: http.StatusInternalServerError, body: "torrent_blocked"})
	h.fake.plan("g2", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	run := h.run(t, false, settings())
	if !slices.Equal(h.fake.writeLog(), []string{"POST /release g1", "POST /release g2"}) {
		t.Errorf("writes = %v", h.fake.writeLog())
	}
	if got := itemResults(run); !slices.Equal(got, []string{"refused", "imported"}) {
		t.Errorf("results = %v", got)
	}
	refused, err := h.db.ListSweepRefused(context.Background(), t0().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(refused) != 1 || refused[0].TitleKey != sweep.NormTitle("Show.S01.DDP.2.0") ||
		!strings.Contains(refused[0].Reason, "500") || !strings.Contains(refused[0].Reason, "code=torrent_blocked") {
		t.Errorf("refused = %+v", refused)
	}
}

func TestRun_PostTimeoutButGrabbed_NoSecondCandidate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1),
		seasonRel("g1", "Show S01 A", "Zilean", 10),
		seasonRel("g2", "Show S01 B", "Zilean", 20))
	h.fake.plan("g1", grabPlan{status: http.StatusGatewayTimeout, grabbed: true})
	run := h.run(t, false, settings())
	if !slices.Equal(h.fake.writeLog(), []string{"POST /release g1"}) {
		t.Errorf("writes = %v", h.fake.writeLog())
	}
	if got := itemResults(run); !slices.Equal(got, []string{"downloading"}) {
		t.Errorf("results = %v", got)
	}
}

func TestRun_ArchiveNoImport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01 A", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "archive", after: 1})
	run := h.run(t, false, settings())
	want := []string{"POST /release g1", "DELETE /queue/12?blocklist=true&removeFromClient=true&skipRedownload=true"}
	if !slices.Equal(h.fake.writeLog(), want) {
		t.Errorf("writes = %v", h.fake.writeLog())
	}
	if got := itemResults(run); !slices.Equal(got, []string{"archive"}) {
		t.Errorf("results = %v", got)
	}
}

func TestRun_ArchiveWithImport(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01 A", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "archive", after: 1, imported: true})
	run := h.run(t, false, settings())
	want := []string{"POST /release g1", "DELETE /queue/12?blocklist=true&removeFromClient=false&skipRedownload=true"}
	if !slices.Equal(h.fake.writeLog(), want) {
		t.Errorf("writes = %v", h.fake.writeLog())
	}
	if got := itemResults(run); !slices.Equal(got, []string{"archive"}) {
		t.Errorf("results = %v", got)
	}
}

func TestRun_TimeoutLeavesDownloading(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12, 13)...)
	h.fake.addSeries(2, "Other", missingEps(2, 1, 31, 32)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01 A", "Zilean", 10))
	h.fake.setReleases(seasonKey(2, 1), seasonRel("g2", "Other S01", "Zilean", 10))
	h.fake.plan("g2", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	start := h.clock.Now()
	run := h.run(t, false, settings())
	if got := itemResults(run); !slices.Equal(got, []string{"downloading", "imported"}) {
		t.Errorf("results = %v", got)
	}
	if run.Counts.Downloading != 1 {
		t.Errorf("counts = %+v", run.Counts)
	}
	times := h.fake.grabTimes
	if len(times) != 2 || times[0].Sub(start) > time.Minute || times[1].Sub(times[0]) < 7*time.Minute {
		t.Errorf("grab times = %v (start %v)", times, start)
	}
}

func TestRun_MaxGrabsStopsPackPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := 1; i <= 3; i++ {
		h.fake.addSeries(i, "Show", missingEps(i, 1, i*10+1, i*10+2)...)
		n := strconv.Itoa(i)
		h.fake.setReleases(seasonKey(i, 1), seasonRel("g"+n, "Show "+n+" S01", "Zilean", 10))
	}
	s := settings()
	s.MaxGrabs = 2
	run := h.run(t, false, s)
	if got := h.fake.countWrites("POST /release"); got != 2 {
		t.Errorf("POSTs = %d, want 2", got)
	}
	if run.Status != "done" || run.Counts.Grabs != 2 || run.Counts.Lookups != 2 {
		t.Errorf("status %s counts %+v", run.Status, run.Counts)
	}
}

func TestRun_MaxLookupsStopsPackPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := 1; i <= 3; i++ {
		h.fake.addSeries(i, "Show", missingEps(i, 1, i*10+1, i*10+2)...)
	}
	s := settings()
	s.MaxLookups = 2
	run := h.run(t, false, s)
	total := 0
	for i := 1; i <= 3; i++ {
		total += h.fake.lookupCount(seasonKey(i, 1))
	}
	if total != 2 || run.Counts.Lookups != 2 || run.Counts.NoPack != 2 {
		t.Errorf("lookups = %d, counts %+v", total, run.Counts)
	}
}

func TestRun_PauseMidRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12, 13)...)
	h.fake.addSeries(2, "Other", missingEps(2, 1, 31, 32)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.setReleases(seasonKey(2, 1), seasonRel("g2", "Other S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "archive", after: 1})
	h.fake.beforePost = func() { h.setPaused(t, true) }
	run := h.run(t, false, settings())
	if !slices.Equal(h.fake.writeLog(), []string{"POST /release g1"}) {
		t.Errorf("writes = %v", h.fake.writeLog())
	}
	if run.Status != "paused" {
		t.Errorf("status = %s", run.Status)
	}
}

func TestRun_GetRetriesThroughOutage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.seriesDownTil = t0().Add(3 * time.Minute)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	run := h.run(t, false, settings())
	if run.Status != "done" || run.Counts.Imported != 1 {
		t.Errorf("status %s (%s) counts %+v", run.Status, run.Reason, run.Counts)
	}
}

func TestRun_GetFailsAfterTenMinutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.seriesDownTil = t0().Add(time.Hour)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	run := h.run(t, false, settings())
	if run.Status != "failed" || !strings.Contains(run.Reason, "503") {
		t.Errorf("status %s (%s)", run.Status, run.Reason)
	}
	if elapsed := h.clock.Now().Sub(t0()); elapsed < 10*time.Minute || elapsed > 11*time.Minute {
		t.Errorf("gave up after %v", elapsed)
	}
}

func TestRun_ReleaseNotRetriedOnTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.runner.Sonarr.SetSlowTimeout(50 * time.Millisecond)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12, 13)...)
	h.fake.addSeries(2, "Other", missingEps(2, 1, 31, 32)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.setReleases(seasonKey(2, 1), seasonRel("g2", "Other S01", "Zilean", 10))
	h.fake.releaseDelay[seasonKey(1, 1)] = 300 * time.Millisecond
	h.fake.plan("g2", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	run := h.run(t, false, settings())
	if got := h.fake.lookupCount(seasonKey(1, 1)); got != 1 {
		t.Errorf("lookups for timed-out season = %d, want 1", got)
	}
	if got := itemResults(run); !slices.Equal(got, []string{"failed", "imported"}) {
		t.Errorf("results = %v", got)
	}
	if run.Status != "done" {
		t.Errorf("status = %s (%s)", run.Status, run.Reason)
	}
}

func TestRun_SkipSet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	old, err := h.db.CreateSweepRun(ctx, "schedule", false, t0().Add(-10*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.db.FinishSweepRun(ctx, old, "done", "", db.SweepCounts{}, t0().Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	season := 1
	item := db.SweepItem{RunID: old, Target: "s:1:1", SeriesID: 1, SeriesTitle: "Show", Season: &season,
		Result: "no_pack", At: t0().Add(-10 * 24 * time.Hour)}
	if err = h.db.AddSweepItem(ctx, &item); err != nil {
		t.Fatal(err)
	}
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.run(t, false, settings())
	if got := h.fake.lookupCount(seasonKey(1, 1)); got != 0 {
		t.Errorf("lookups = %d, want 0 (no_pack 10 days ago)", got)
	}
}

func TestRun_ItemWrittenBeforePost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	var seen []string
	h.fake.beforePost = func() {
		runs, err := h.db.ListSweepRuns(context.Background(), 1)
		if err != nil || len(runs) == 0 {
			t.Errorf("list runs: %v", err)
			return
		}
		seen = itemResults(runs[0])
	}
	h.run(t, false, settings())
	if !slices.Equal(seen, []string{"grabbing"}) {
		t.Errorf("items at POST time = %v, want [grabbing]", seen)
	}
}

func TestRun_DryRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12, 13)...)
	h.fake.addSeries(2, "Other", missingEps(2, 1, 31, 32)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	run := h.run(t, true, settings())
	if len(h.fake.writeLog()) != 0 {
		t.Errorf("dry run wrote %v", h.fake.writeLog())
	}
	if got := itemResults(run); !slices.Equal(got, []string{"would_grab", "no_pack"}) {
		t.Errorf("results = %v", got)
	}
	if !run.Dry || run.Counts.Grabs != 1 {
		t.Errorf("dry %v counts %+v", run.Dry, run.Counts)
	}
	skip, err := h.db.NoPackTargets(context.Background(), t0().Add(-time.Hour))
	if err != nil || len(skip) != 0 {
		t.Errorf("NoPackTargets after dry run = %v, %v", skip, err)
	}
}

func TestRun_HistoryQueriedFromFiveMinutesBefore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 2})
	h.run(t, false, settings())
	if len(h.fake.grabTimes) != 1 || len(h.fake.historyDates) == 0 {
		t.Fatalf("grabs %v history queries %v", h.fake.grabTimes, h.fake.historyDates)
	}
	for _, d := range h.fake.historyDates {
		if d.After(h.fake.grabTimes[0].Add(-5 * time.Minute)) {
			t.Errorf("history since %v, grab at %v", d, h.fake.grabTimes[0])
		}
	}
}

func searchSettings() sweep.Settings {
	s := settings()
	s.MaxSearches = 50
	return s
}

func TestRun_AnimeUsesEpisodeLookupsOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addAnime(1, "Anime", missingEps(1, 1, 1, 2, 3)...)
	h.fake.setReleases("e1", animeRel("Anime 01-03", 1, 2, 3))
	h.fake.plan("Anime 01-03", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	run := h.run(t, false, searchSettings())
	h.fake.mu.Lock()
	lookups := maps.Clone(h.fake.lookups)
	h.fake.mu.Unlock()
	if !maps.Equal(lookups, map[string]int{"e1": 1}) {
		t.Errorf("lookups = %v, want only e1", lookups)
	}
	for _, w := range h.fake.writeLog() {
		if strings.Contains(w, "SeasonSearch") {
			t.Errorf("anime got a season search: %s", w)
		}
	}
	if got := itemResults(run); !slices.Equal(got, []string{"imported"}) || run.Items[0].Target != "e:1" {
		t.Errorf("items = %+v", run.Items)
	}
}

func TestRun_AnimeBatchMarksCovered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addAnime(1, "Anime", missingEps(1, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)...)
	h.fake.setReleases("e1", animeRel("Anime 02-10", 2, 3, 4, 5, 6, 7, 8, 9, 10))
	h.fake.plan("Anime 02-10", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	run := h.run(t, false, settings())
	for id := 2; id <= 10; id++ {
		if n := h.fake.lookupCount("e" + strconv.Itoa(id)); n != 0 {
			t.Errorf("episode %d looked up %d times after the batch covered it", id, n)
		}
	}
	if got := itemResults(run); !slices.Equal(got, []string{"imported"}) {
		t.Errorf("results = %v", got)
	}
}

func TestRun_AnimeOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addAnime(1, "Anime", missingEps(1, 1, 1, 2, 3)...)
	h.fake.setReleases("e1", animeRel("Anime 01-03", 1, 2, 3))
	s := searchSettings()
	s.Anime = false
	h.run(t, false, s)
	h.fake.mu.Lock()
	lookups := len(h.fake.lookups)
	h.fake.mu.Unlock()
	if lookups != 0 || len(h.fake.writeLog()) != 0 {
		t.Errorf("anime off: lookups %d, writes %v", lookups, h.fake.writeLog())
	}
}

func TestRun_EpisodePassWaitsForCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", append(missingEps(1, 1, 11, 12), missingEps(1, 2, 21, 22)...)...)
	h.fake.addAnime(2, "Anime", missingEps(2, 1, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112)...)
	h.fake.commandNeeds = 3
	run := h.run(t, false, searchSettings())
	want := [][]int{{11, 12}, {21, 22}, {101, 102, 103, 104, 105, 106, 107, 108, 109, 110}, {111, 112}}
	if got := h.fake.commands(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	h.fake.mu.Lock()
	times, done := h.fake.commandTimes, maps.Clone(h.fake.completedAt)
	h.fake.mu.Unlock()
	for i := 1; i < len(times); i++ {
		prev, ok := done[100+i]
		if !ok || times[i].Sub(prev) < time.Minute {
			t.Errorf("command %d sent at %v, previous completed at %v (seen %v)", i, times[i], prev, ok)
		}
	}
	if run.Counts.Searched != 16 || run.Status != "done" {
		t.Errorf("status %s counts %+v", run.Status, run.Counts)
	}
	if got := itemResults(run); slices.Index(got, "searched") < 0 {
		t.Errorf("results = %v, want searched items", got)
	}
}

func TestRun_MaxSearchesCountsEpisodes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ids := make([]int, 0, 25)
	for i := 1; i <= 25; i++ {
		ids = append(ids, i)
	}
	h.fake.addSeries(1, "Show", append(missingEps(1, 1, ids[:15]...), missingEps(1, 2, ids[15:]...)...)...)
	s := settings()
	s.MaxSearches = 20
	run := h.run(t, false, s)
	total := 0
	for _, c := range h.fake.commands() {
		total += len(c)
	}
	if total != 20 || run.Counts.Searched != 20 {
		t.Errorf("ids sent %d, searched %d, want 20", total, run.Counts.Searched)
	}
}

func TestRun_LeftoverCleanupAllMatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1, leftover: []string{
		"Not an upgrade for existing episode file(s). Existing quality: WEBDL-1080p.",
		"Not a Custom Format upgrade for existing episode file(s). New: [] do not improve on Existing: []",
	}})
	h.fake.deleteStatus = http.StatusNotFound
	run := h.run(t, false, settings())
	var deletes []string
	for _, w := range h.fake.writeLog() {
		if strings.HasPrefix(w, "DELETE") {
			deletes = append(deletes, w)
		}
	}
	if len(deletes) != 1 || !strings.HasPrefix(deletes[0], "DELETE /queue/3") ||
		!strings.HasSuffix(deletes[0], "?blocklist=false&removeFromClient=false&skipRedownload=false") {
		t.Errorf("deletes = %v", deletes)
	}
	if run.Status != "done" {
		t.Errorf("status = %s (%s), 404 should be tolerated", run.Status, run.Reason)
	}
}

func TestRun_LeftoverCleanupMixedSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Zilean", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1, leftover: []string{
		"Not an upgrade for existing episode file(s). Existing quality: WEBDL-1080p.",
		"No files found are eligible for import",
	}})
	h.run(t, false, settings())
	if n := h.fake.countWrites("DELETE"); n != 0 {
		t.Errorf("writes = %v, want no DELETE", h.fake.writeLog())
	}
}

func TestRun_ExcludedSeriesUntouched(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Excluded", missingEps(1, 1, 11, 12)...)
	h.fake.addAnime(2, "Excluded anime", missingEps(2, 1, 21, 22)...)
	h.fake.addSeries(3, "Kept", missingEps(3, 1, 31, 32)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Excluded S01", "Zilean", 10))
	h.fake.setReleases("e21", animeRel("Excluded anime 01-02", 21, 22))
	s := searchSettings()
	s.ExcludedSeries = []int{1, 2}
	h.run(t, false, s)
	h.fake.mu.Lock()
	lookups := maps.Clone(h.fake.lookups)
	h.fake.mu.Unlock()
	if !maps.Equal(lookups, map[string]int{seasonKey(3, 1): 1}) {
		t.Errorf("lookups = %v, want only the kept series", lookups)
	}
	if n := h.fake.countWrites("POST /release"); n != 0 {
		t.Errorf("grabs = %d", n)
	}
	if got := h.fake.commands(); !slices.EqualFunc(got, [][]int{{31, 32}}, slices.Equal) {
		t.Errorf("commands = %v, want only the kept series", got)
	}
}

func TestRun_DryRunEpisodePass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.addAnime(2, "Anime", missingEps(2, 1, 1, 2, 3, 4)...)
	h.fake.setReleases("e1", animeRel("Anime 01-03", 1, 2, 3))
	run := h.run(t, true, searchSettings())
	if len(h.fake.writeLog()) != 0 {
		t.Errorf("dry run wrote %v", h.fake.writeLog())
	}
	want := []string{"no_pack", "would_grab", "no_pack", "would_search", "would_search"}
	if got := itemResults(run); !slices.Equal(got, want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
	if run.Items[4].Episodes != "4" || run.Counts.Searched != 3 {
		t.Errorf("anime search item %+v, counts %+v", run.Items[4], run.Counts)
	}
}

func TestRun_ProviderErrorRetriedNextRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show.S01.1080p", "Comet", 10))
	h.fake.plan("g1", grabPlan{status: http.StatusInternalServerError, body: `{"code": "provider_error"}`})
	if got := itemResults(h.run(t, false, settings())); !slices.Equal(got, []string{"refused"}) {
		t.Fatalf("run 1 results = %v", got)
	}
	h.clock.mu.Lock()
	h.clock.t = h.clock.t.Add(7 * 24 * time.Hour)
	h.clock.mu.Unlock()
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	h.run(t, false, settings())
	if n := h.fake.countWrites("POST /release g1"); n != 2 {
		t.Errorf("POST g1 count = %d, want 2 (provider_error is retried next run); writes %v", n, h.fake.writeLog())
	}
}
