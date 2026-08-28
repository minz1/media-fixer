package agent_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register SQLite driver for the raw-backdate test helper

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

func newTestAgentDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// TestAttemptHistory_EmptyForFreshIncident confirms a brand-new incident with
// no logged actions gets no history block at all — nothing to remember yet.
func TestAttemptHistory_EmptyForFreshIncident(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, slog.New(slog.DiscardHandler))
	if got := a.AttemptHistoryForTest(ctx, inc.ID); got != "" {
		t.Errorf("expected empty history for a fresh incident, got %q", got)
	}
}

// TestAttemptHistory_RendersPriorActions is the regression test for the "no
// memory across attempts" bug: a resumed diagnosis must be told what was
// already tried on this incident and that it didn't work, by name and target.
func TestAttemptHistory_RendersPriorActions(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusReopened,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Backrooms",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "clear_jellyfin_cache",
		Params:      map[string]any{"item_id": "21e909427236eaf5de52bc19834f51c4"},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, slog.New(slog.DiscardHandler))
	got := a.AttemptHistoryForTest(ctx, inc.ID)

	for _, want := range []string{"clear_jellyfin_cache", "21e909427236eaf5de52bc19834f51c4", "did NOT resolve"} {
		if !strings.Contains(got, want) {
			t.Errorf("attempt history missing %q; got:\n%s", want, got)
		}
	}
}

// TestBuildSummarySeed_LabelsFindingsUnverifiedAndRequiresFullProtocol is the
// regression test for the "100 meters" rerun bug: a resumed run cited stale,
// unrelated ffprobe errors from a two-day-old summary as its root cause,
// because the seed only mandated refreshing two of the five protocol steps
// and presented the summary as settled fact. The seed must label prior
// findings as unverified, require the full protocol (not just loki+disk),
// and require the eventual diagnosis to cite evidence from the CURRENT run.
func TestBuildSummarySeed_LabelsFindingsUnverifiedAndRequiresFullProtocol(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	a := agent.New(nil, "", nil, database, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusReopened,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "100 meters",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	seed := a.BuildSummarySeed(ctx, inc, "ffprobe failed for President.Curtis.S01E05")
	if len(seed) != 2 {
		t.Fatalf("expected [system, user], got %d messages", len(seed))
	}
	got := seed[1].Content

	for _, want := range []string{
		"UNVERIFIED prior inference",
		"NOT established fact",
		"FULL protocol",
		"dd_readability_test",
		"arr_media_status",
		"MUST cite evidence YOU observed in this run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary seed missing %q; got:\n%s", want, got)
		}
	}
}

// TestActionAlreadyApplied is the regression test for the structural
// backstop: an action already logged as applied against a given target on
// this incident must be recognized as a repeat, while an action against a
// different target (or a different incident) must not.
func TestActionAlreadyApplied(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusReopened,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Backrooms",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	other := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Other"}
	if err := database.CreateIncident(ctx, other); err != nil {
		t.Fatal(err)
	}

	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "clear_jellyfin_cache",
		Params:      map[string]any{"item_id": "item-a"},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, slog.New(slog.DiscardHandler))

	if !a.ActionAlreadyAppliedForTest(ctx, inc.ID, "clear_jellyfin_cache", `{"item_id":"item-a"}`) {
		t.Error("expected a repeat of the same action+target to be detected")
	}
	if a.ActionAlreadyAppliedForTest(ctx, inc.ID, "clear_jellyfin_cache", `{"item_id":"item-b"}`) {
		t.Error("a different target should not be flagged as a repeat")
	}
	if a.ActionAlreadyAppliedForTest(ctx, inc.ID, "restart_jellyfin", `{}`) {
		t.Error("a different action should not be flagged as a repeat")
	}
	if a.ActionAlreadyAppliedForTest(ctx, other.ID, "clear_jellyfin_cache", `{"item_id":"item-a"}`) {
		t.Error("the same action+target on a different incident should not be flagged as a repeat")
	}
}

// TestActionAlreadyApplied_DifferentEpisodesSameSeries_DoNotCollide is the
// direct regression test for the identityParamsMatch bug: checking only the
// first identity key found (title, which two arr_search_missing calls for
// different episodes of the same series share) wrongly blocked the second
// call as a duplicate before it ever ran. season/episode must also be
// compared for a shared title to mean the same target.
func TestActionAlreadyApplied_DifferentEpisodesSameSeries_DoNotCollide(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusReopened,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Rick and Morty",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID,
		Action:     "arr_search_missing",
		Params: map[string]any{
			"media_type": "tv", "title": "Rick and Morty", "season": float64(9), "episode": float64(9),
		},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, slog.New(slog.DiscardHandler))

	// Same series, same episode: this IS a repeat.
	if !a.ActionAlreadyAppliedForTest(ctx, inc.ID, "arr_search_missing",
		`{"media_type":"tv","title":"Rick and Morty","season":9,"episode":9}`) {
		t.Error("expected a repeat of the same series+season+episode to be detected")
	}
	// Same series, different episode: must NOT be blocked as a duplicate.
	if a.ActionAlreadyAppliedForTest(ctx, inc.ID, "arr_search_missing",
		`{"media_type":"tv","title":"Rick and Morty","season":9,"episode":10}`) {
		t.Error("a different episode of the same series was wrongly flagged as a repeat")
	}
	// Same series, different season: must NOT be blocked as a duplicate.
	if a.ActionAlreadyAppliedForTest(ctx, inc.ID, "arr_search_missing",
		`{"media_type":"tv","title":"Rick and Morty","season":10,"episode":9}`) {
		t.Error("a different season of the same series was wrongly flagged as a repeat")
	}
}

// TestActionAlreadyApplied_ParameterlessAction confirms two parameterless
// invocations of the same tool (e.g. restart_jellyfin, which always logs nil
// params) match each other as a repeat.
func TestActionAlreadyApplied_ParameterlessAction(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusReopened, Source: "discord", ReportedBy: "x", What: "other", Title: "Infra"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "restart_jellyfin",
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, slog.New(slog.DiscardHandler))
	if !a.ActionAlreadyAppliedForTest(ctx, inc.ID, "restart_jellyfin", `{}`) {
		t.Error("expected a repeat parameterless restart_jellyfin to be detected")
	}
}

// verifyTestServers wires a fake Jellyfin and media-agent so VerifyResolved
// tests can control exactly what "current state" looks like. ddBytesRead and
// ddError are read on every /dd-test call, so a test can flip them between
// two captures to simulate a fix actually landing (or not).
func verifyTestServers(t *testing.T, ddBytesRead *atomic.Int64, ddError *atomic.Value) *agent.Dispatcher {
	t.Helper()

	jellyfinMux := http.NewServeMux()
	jellyfinMux.HandleFunc("/Users", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "admin-1"}})
	})
	jellyfinMux.HandleFunc("/Items/item1/PlaybackInfo", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{
			MediaSources: []client.MediaSource{{ID: "src1", Path: "/mnt/decypharr/foo.mkv"}},
		})
	})
	jellyfinMux.HandleFunc("/Shows/item1/Episodes", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{})
	})
	jellyfinSrv := httptest.NewServer(jellyfinMux)
	t.Cleanup(jellyfinSrv.Close)

	mediaMux := http.NewServeMux()
	mediaMux.HandleFunc("/dd-test", func(w http.ResponseWriter, _ *http.Request) {
		errMsg, _ := ddError.Load().(string)
		_ = json.NewEncoder(w).Encode(mediaagentapi.DDTestResult{
			BytesRead: ddBytesRead.Load(),
			Error:     errMsg,
		})
	})
	mediaSrv := httptest.NewServer(mediaMux)
	t.Cleanup(mediaSrv.Close)

	return &agent.Dispatcher{
		Jellyfin:   client.NewJellyfin(jellyfinSrv.URL, "key"),
		MediaAgent: client.NewMediaAgent(mediaSrv.URL, ""),
	}
}

// TestVerifyResolved_IdenticalSignature_ReturnsFalse is the core regression
// test for the "verification is a tautology" bug: a fix that leaves the item
// in exactly the same observable state it was in before — the same failure
// mode every "agent fixed" claim in a week of production logs actually hit —
// must not be reported as verified just because the item still looks
// healthy in isolation.
func TestVerifyResolved_IdenticalSignature_ReturnsFalse(t *testing.T) {
	t.Parallel()
	var ddBytesRead atomic.Int64
	var ddError atomic.Value
	ddBytesRead.Store(104_857_600) // reads as healthy both times — nothing changes
	ddError.Store("")

	disp := verifyTestServers(t, &ddBytesRead, &ddError)
	a := agent.New(nil, "", disp, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	pre := a.CaptureSignatureForTest(ctx, "item1", "")
	if pre.DDBytesRead == 0 {
		t.Fatalf("test setup: expected a healthy baseline, got %+v", pre)
	}
	if a.VerifyResolved(ctx, "item1", "", pre) {
		t.Error("expected VerifyResolved to be false when the signature is unchanged")
	}
}

// TestVerifyResolved_ChangedSignature_ReturnsTrue confirms a genuine change —
// a dd read that was failing now succeeds — is recognized as verified.
func TestVerifyResolved_ChangedSignature_ReturnsTrue(t *testing.T) {
	t.Parallel()
	var ddBytesRead atomic.Int64
	var ddError atomic.Value
	ddError.Store("input/output error")

	disp := verifyTestServers(t, &ddBytesRead, &ddError)
	a := agent.New(nil, "", disp, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	pre := a.CaptureSignatureForTest(ctx, "item1", "")
	if pre.DDError == "" {
		t.Fatalf("test setup: expected a broken baseline, got %+v", pre)
	}

	ddError.Store("")
	ddBytesRead.Store(104_857_600)

	if !a.VerifyResolved(ctx, "item1", "", pre) {
		t.Error("expected VerifyResolved to be true once the signature actually changed")
	}
}

// TestVerifyResolved_NilBaseline_StillRequiresReadability confirms that with
// no baseline to compare against (e.g. the incident's item ID was never
// known up front), VerifyResolved still requires the item to actually be
// usable — it doesn't just pass by default.
func TestVerifyResolved_NilBaseline_StillRequiresReadability(t *testing.T) {
	t.Parallel()
	var ddBytesRead atomic.Int64
	var ddError atomic.Value
	ddError.Store("input/output error")

	disp := verifyTestServers(t, &ddBytesRead, &ddError)
	a := agent.New(nil, "", disp, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	if a.VerifyResolved(ctx, "item1", "", nil) {
		t.Error("expected VerifyResolved to be false when the file still isn't readable")
	}

	ddError.Store("")
	ddBytesRead.Store(104_857_600)
	if !a.VerifyResolved(ctx, "item1", "", nil) {
		t.Error("expected VerifyResolved to be true once the file is readable")
	}
}

// TestVerifyResolved_ArrConfirmsMissing_HardGateBlocksVerification is the
// direct regression test for the Rick and Morty S09E09 false-positive: the
// Jellyfin/dd side can report a fully healthy signature (item1's fixture: a
// good dd read, a real path) while *arr confirms the title is still missing
// a file — and that must be a hard "not verified", not just one signal among
// several the Jellyfin side could outvote.
func TestVerifyResolved_ArrConfirmsMissing_HardGateBlocksVerification(t *testing.T) {
	t.Parallel()
	var ddBytesRead atomic.Int64
	var ddError atomic.Value
	ddBytesRead.Store(104_857_600)
	ddError.Store("")
	disp := verifyTestServers(t, &ddBytesRead, &ddError)

	// Rick and Morty's fixture has episode 9 of season 9 with no file, so
	// the whole-series aggregate arrHasFileForTitle resolves is false.
	arrSrv, _ := newArrStatusFixtureServer(t)
	defer arrSrv.Close()
	disp.Sonarr = client.NewArr(arrSrv.URL, "key")

	a := agent.New(nil, "", disp, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	if a.VerifyResolved(ctx, "item1", "Rick and Morty", nil) {
		t.Error("expected VerifyResolved to be false when *arr confirms the title is missing a file, " +
			"even though the Jellyfin/dd signature looks fully healthy")
	}
}

// TestImproved_DirectionAware is the direct regression test for the
// "direction-agnostic" bug: the old check (*post != *pre) passed on ANY
// difference, including an episode count that went DOWN — content actively
// disappearing satisfied "something changed" just as well as a real fix did.
func TestImproved_DirectionAware(t *testing.T) {
	t.Parallel()

	t.Run("episode count decrease is not improved", func(t *testing.T) {
		t.Parallel()
		pre := &agent.FixSignature{EpisodeCount: 10}
		post := &agent.FixSignature{EpisodeCount: 9}
		if agent.ImprovedForTest(pre, post) {
			t.Error("a decreased episode count must not count as improved")
		}
	})

	t.Run("episode count increase is improved", func(t *testing.T) {
		t.Parallel()
		pre := &agent.FixSignature{EpisodeCount: 9}
		post := &agent.FixSignature{EpisodeCount: 10}
		if !agent.ImprovedForTest(pre, post) {
			t.Error("an increased episode count should count as improved")
		}
	})

	t.Run("identical episode count is not improved", func(t *testing.T) {
		t.Parallel()
		pre := &agent.FixSignature{EpisodeCount: 9}
		post := &agent.FixSignature{EpisodeCount: 9}
		if agent.ImprovedForTest(pre, post) {
			t.Error("an unchanged episode count must not count as improved")
		}
	})

	t.Run("arr newly confirming a file is improved", func(t *testing.T) {
		t.Parallel()
		pre := &agent.FixSignature{ArrChecked: true, ArrHasFile: false}
		post := &agent.FixSignature{ArrChecked: true, ArrHasFile: true}
		if !agent.ImprovedForTest(pre, post) {
			t.Error("arr confirming a file where there wasn't one should count as improved")
		}
	})

	t.Run("a path that changed but doesn't read is not improved", func(t *testing.T) {
		t.Parallel()
		pre := &agent.FixSignature{Path: "/a.mkv", DDError: "input/output error"}
		post := &agent.FixSignature{Path: "/b.mkv", DDError: "input/output error"}
		if agent.ImprovedForTest(pre, post) {
			t.Error("a different but still-unreadable path must not count as improved")
		}
	})
}

// TestDisruptionNote covers the three cases that must produce no warning
// (nothing recorded yet, the disruption was this same incident's own action,
// the disruption is stale) plus the one case that must (a recent disruption
// by a different incident) — the exact set of conditions
// runManager.globalSlot's aftershock-visibility layer depends on.
func TestDisruptionNote(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	dbPath := f.Name()
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	a := agent.New(nil, "", nil, database, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	if note := a.DisruptionNoteForTest(ctx, "11111111-1111-1111-1111-111111111111"); note != "" {
		t.Fatalf("expected no note before any disruption recorded, got %q", note)
	}

	if recordErr := database.RecordDisruption(
		ctx,
		"restart_jellyfin",
		"11111111-1111-1111-1111-111111111111",
	); recordErr != nil {
		t.Fatal(recordErr)
	}
	if note := a.DisruptionNoteForTest(ctx, "11111111-1111-1111-1111-111111111111"); note != "" {
		t.Fatalf("expected no note about an incident's own action, got %q", note)
	}

	if recordErr := database.RecordDisruption(
		ctx,
		"restart_jellyfin",
		"22222222-2222-2222-2222-222222222222",
	); recordErr != nil {
		t.Fatal(recordErr)
	}
	gotNote := a.DisruptionNoteForTest(ctx, "11111111-1111-1111-1111-111111111111")
	if !strings.Contains(gotNote, "restart_jellyfin") || !strings.Contains(gotNote, "22222222") {
		t.Fatalf("expected a note naming the action and incident, got %q", gotNote)
	}

	// A stale disruption (older than disruptionQuietWindow) must not warn —
	// simulated via a second raw connection to backdate the timestamp, since
	// RecordDisruption always stamps time.Now() (same pattern as
	// TestSweepStaleRuns_RerunsHungIncident in internal/incident/service_test.go).
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := raw.ExecContext(ctx, `UPDATE last_disruption SET at = ?`,
		time.Now().Add(-10*time.Minute)); execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if note := a.DisruptionNoteForTest(ctx, "11111111-1111-1111-1111-111111111111"); note != "" {
		t.Fatalf("expected no note for a stale disruption, got %q", note)
	}
}

// lokiCapturingServer starts a fake Loki that records the query params of the
// last query_range request it received.
func lokiCapturingServer(t *testing.T) (*client.LokiClient, *url.Values) {
	t.Helper()
	var got url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []any{}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	loki, err := client.NewLoki(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return loki, &got
}

// TestLokiQuery_FilterAndIncidentWindow is the regression test for "loki
// evidence is ambient noise": a filter must be embedded as a LogQL line
// filter, and by default the window must be centered on the incident's
// report time (Dispatcher.IncidentTime), not on [time.Now] — the bug that
// made re-diagnosing a two-day-old incident grep a window with no
// relationship to when it actually failed.
func TestLokiQuery_FilterAndIncidentWindow(t *testing.T) {
	t.Parallel()
	loki, got := lokiCapturingServer(t)

	incidentTime := time.Date(2026, 8, 26, 3, 11, 0, 0, time.UTC)
	disp := &agent.Dispatcher{Loki: loki, IncidentTime: incidentTime}

	if _, err := disp.Call(context.Background(), "loki_query", map[string]any{
		"units":        `{unit=~"jellyfin.service|decypharr.service"}`,
		"minutes_back": float64(30),
		"filter":       "Backrooms",
	}); err != nil {
		t.Fatal(err)
	}

	if q := got.Get("query"); !strings.Contains(q, `|= "Backrooms"`) {
		t.Errorf("query missing line filter: %q", q)
	}

	start, end := parseLokiWindow(t, *got)
	wantStart, wantEnd := incidentTime.Add(-15*time.Minute), incidentTime.Add(15*time.Minute)
	if diff := start.Sub(wantStart); diff < -time.Second || diff > time.Second {
		t.Errorf("start = %v, want ~%v", start, wantStart)
	}
	if diff := end.Sub(wantEnd); diff < -time.Second || diff > time.Second {
		t.Errorf("end = %v, want ~%v", end, wantEnd)
	}
}

// TestLokiQuery_AroundIncidentFalse_UsesNow confirms around_incident=false
// deliberately overrides the incident-anchored default.
func TestLokiQuery_AroundIncidentFalse_UsesNow(t *testing.T) {
	t.Parallel()
	loki, got := lokiCapturingServer(t)

	disp := &agent.Dispatcher{Loki: loki, IncidentTime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}

	before := time.Now()
	if _, err := disp.Call(context.Background(), "loki_query", map[string]any{
		"units":           `{unit=~"jellyfin.service"}`,
		"minutes_back":    float64(30),
		"around_incident": false,
	}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	_, end := parseLokiWindow(t, *got)
	if end.Before(before.Add(-time.Second)) || end.After(after.Add(time.Second)) {
		t.Errorf("end = %v, want between %v and %v (anchored to now, not the 2020 incident time)", end, before, after)
	}
}

func parseLokiWindow(t *testing.T, q url.Values) (time.Time, time.Time) {
	t.Helper()
	startNS, err := strconv.ParseInt(q.Get("start"), 10, 64)
	if err != nil {
		t.Fatalf("parse start: %v", err)
	}
	endNS, err := strconv.ParseInt(q.Get("end"), 10, 64)
	if err != nil {
		t.Fatalf("parse end: %v", err)
	}
	return time.Unix(0, startNS), time.Unix(0, endNS)
}
