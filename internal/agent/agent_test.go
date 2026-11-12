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
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register SQLite driver for the raw-backdate test helper

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
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
	lifecycletest.Incident(t, database, inc)

	a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))
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
	lifecycletest.Incident(t, database, inc)
	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "clear_jellyfin_cache",
		Params:      map[string]any{"item_id": "21e909427236eaf5de52bc19834f51c4"},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))
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
	a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusReopened,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "100 meters",
	}
	lifecycletest.Incident(t, database, inc)

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
	lifecycletest.Incident(t, database, inc)
	other := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Other"}
	lifecycletest.Incident(t, database, other)

	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "clear_jellyfin_cache",
		Params:      map[string]any{"item_id": "item-a"},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))

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
	lifecycletest.Incident(t, database, inc)
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

	a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))

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
	lifecycletest.Incident(t, database, inc)
	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "restart_jellyfin",
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}); err != nil {
		t.Fatal(err)
	}

	a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))
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
	a := agent.New(nil, "", disp, nil, nil, slog.New(slog.DiscardHandler))
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
	a := agent.New(nil, "", disp, nil, nil, slog.New(slog.DiscardHandler))
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
	a := agent.New(nil, "", disp, nil, nil, slog.New(slog.DiscardHandler))
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

	a := agent.New(nil, "", disp, nil, nil, slog.New(slog.DiscardHandler))
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
	jrnl := journal.New(database)
	ctx := context.Background()

	const (
		incidentA = "11111111-1111-1111-1111-111111111111"
		incidentB = "22222222-2222-2222-2222-222222222222"
	)
	// LogAction now enforces the incident_id foreign key (Phase 0's pragma
	// fix actually turned it on), unlike the old RecordDisruption, which
	// wrote to a last_disruption row with no such constraint — both fixture
	// incidents need to exist for real.
	for _, id := range []string{incidentA, incidentB} {
		lifecycletest.Incident(t, database, &db.Incident{
			ID: id, Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: id,
		})
	}

	a := agent.New(nil, "", nil, database, jrnl, slog.New(slog.DiscardHandler))

	if note := a.DisruptionNoteForTest(ctx, incidentA); note != "" {
		t.Fatalf("expected no note before any disruption recorded, got %q", note)
	}

	if logErr := jrnl.LogAction(ctx, &db.ActionLog{
		IncidentID: incidentA, Action: "restart_jellyfin", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true); logErr != nil {
		t.Fatal(logErr)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note != "" {
		t.Fatalf("expected no note about an incident's own action, got %q", note)
	}

	if logErr := jrnl.LogAction(ctx, &db.ActionLog{
		IncidentID: incidentB, Action: "restart_jellyfin", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true); logErr != nil {
		t.Fatal(logErr)
	}
	gotNote := a.DisruptionNoteForTest(ctx, incidentA)
	if !strings.Contains(gotNote, "restart_jellyfin") || !strings.Contains(gotNote, "22222222") {
		t.Fatalf("expected a note naming the action and incident, got %q", gotNote)
	}

	// A stale disruption (older than disruptionCap) must not warn —
	// simulated via a second raw connection to backdate the event's
	// timestamp, since LogAction always stamps time.Now() (same pattern as
	// TestSweepStaleRuns_RerunsHungIncident in internal/incident/service_test.go).
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := raw.ExecContext(ctx,
		`UPDATE incident_events SET at = ? WHERE incident_id = ? AND kind = 'action_applied'`,
		time.Now().Add(-40*time.Minute), incidentB,
	); execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note != "" {
		t.Fatalf("expected no note for a stale disruption, got %q", note)
	}
}

func TestE3_RestartSettledGivesNoNote(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	jrnl := journal.New(database)
	ctx := context.Background()
	const (
		incidentA = "11111111-1111-1111-1111-111111111111"
		incidentB = "22222222-2222-2222-2222-222222222222"
	)
	for _, id := range []string{incidentA, incidentB} {
		lifecycletest.Incident(t, database, &db.Incident{
			ID: id, Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: id,
		})
	}
	a := agent.New(nil, "", nil, database, jrnl, slog.New(slog.DiscardHandler))

	if err := jrnl.LogActionGen(ctx, &db.ActionLog{
		IncidentID: incidentB, Action: "restart_jellyfin", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true, 0, "call-1"); err != nil {
		t.Fatal(err)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note == "" {
		t.Fatal("expected a note before the restart settled")
	}
	if err := jrnl.SettleDisruption(ctx, journal.Disruption{IncidentID: incidentB, CallID: "call-1"}); err != nil {
		t.Fatal(err)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note != "" {
		t.Fatalf("expected no note after the restart settled, got %q", note)
	}
}

func TestE3_ScanStillRunningGivesNote(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	jrnl := journal.New(database)
	ctx := context.Background()
	cs, srv := newCallServer(t)
	cs.scanRunning = true
	const (
		incidentA = "11111111-1111-1111-1111-111111111111"
		incidentB = "22222222-2222-2222-2222-222222222222"
	)
	for _, id := range []string{incidentA, incidentB} {
		lifecycletest.Incident(t, database, &db.Incident{
			ID: id, Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: id,
		})
	}
	a := agent.New(nil, "", fullDispatcher(srv.URL), database, jrnl, slog.New(slog.DiscardHandler))

	started := time.Now().Add(-2 * time.Minute)
	if err := jrnl.LogAction(ctx, &db.ActionLog{
		IncidentID: incidentB, Action: "jellyfin_library_scan", TriggeredBy: "agent", Status: db.ActionApplied,
		AppliedAt: &started,
	}, true); err != nil {
		t.Fatal(err)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note == "" {
		t.Fatal("expected a note while the scan is still running")
	}
	cs.scanRunning = false
	if note := a.DisruptionNoteForTest(ctx, incidentA); note != "" {
		t.Fatalf("expected no note once the scan finished, got %q", note)
	}
}

func TestE3_OwnDisruptionNeverNotes(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	jrnl := journal.New(database)
	ctx := context.Background()
	inc := lifecycletest.Incident(t, database, &db.Incident{
		ID: "11111111-1111-1111-1111-111111111111", Status: db.StatusOpen, Source: "discord", ReportedBy: "x",
		What: "cant_play", Title: "own",
	})
	a := agent.New(nil, "", nil, database, jrnl, slog.New(slog.DiscardHandler))
	if err := jrnl.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "restart_jellyfin", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true); err != nil {
		t.Fatal(err)
	}
	if note := a.DisruptionNoteForTest(ctx, inc.ID); note != "" {
		t.Fatalf("expected no note for the incident's own disruption, got %q", note)
	}
}

func TestE3_CapEndsNote(t *testing.T) {
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
	jrnl := journal.New(database)
	ctx := context.Background()
	const (
		incidentA = "11111111-1111-1111-1111-111111111111"
		incidentB = "22222222-2222-2222-2222-222222222222"
	)
	for _, id := range []string{incidentA, incidentB} {
		lifecycletest.Incident(t, database, &db.Incident{
			ID: id, Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: id,
		})
	}
	a := agent.New(nil, "", nil, database, jrnl, slog.New(slog.DiscardHandler))
	if logErr := jrnl.LogAction(ctx, &db.ActionLog{
		IncidentID: incidentB, Action: "refresh_decypharr_links", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true); logErr != nil {
		t.Fatal(logErr)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note == "" {
		t.Fatal("expected a note inside the cap for a disruption with no settle signal")
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := raw.ExecContext(ctx,
		`UPDATE incident_events SET at = ? WHERE incident_id = ? AND kind = 'action_applied'`,
		time.Now().Add(-40*time.Minute), incidentB,
	); execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note != "" {
		t.Fatalf("expected the 30-minute cap to end the note, got %q", note)
	}
}

// lokiCapturingServer starts a fake Loki that records the query params of the
// every query_range request it received; got is the last one.
func lokiCapturingServer(t *testing.T) (*client.LokiClient, *url.Values, *[]url.Values) {
	t.Helper()
	var got url.Values
	var all []url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		all = append(all, got)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []any{}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	loki, err := client.NewLoki(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return loki, &got, &all
}

// TestScope_LokiAnchorsOnReportedAt is the regression test for "loki
// evidence is ambient noise": a filter must be embedded as a LogQL line
// filter, and by default the window must be centered on the incident's
// report time (Scope.ReportedAt), not on [time.Now] — the bug that
// made re-diagnosing a two-day-old incident grep a window with no
// relationship to when it actually failed.
func TestScope_LokiAnchorsOnReportedAt(t *testing.T) {
	t.Parallel()
	loki, got, _ := lokiCapturingServer(t)

	incidentTime := time.Date(2026, 8, 26, 3, 11, 0, 0, time.UTC)
	disp := &agent.Dispatcher{Loki: loki}

	if _, err := disp.CallScoped(
		context.Background(),
		agent.Scope{ReportedAt: incidentTime},
		"loki_query",
		map[string]any{
			"units":        `{unit=~"jellyfin.service|decypharr.service"}`,
			"minutes_back": float64(30),
			"filter":       "Backrooms",
		},
	); err != nil {
		t.Fatal(err)
	}

	if q := got.Get("query"); !strings.Contains(q, `|~ "(?i)(Backrooms)"`) {
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
	loki, got, _ := lokiCapturingServer(t)

	disp := &agent.Dispatcher{Loki: loki}

	before := time.Now()
	if _, err := disp.CallScoped(
		context.Background(),
		agent.Scope{ReportedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
		"loki_query",
		map[string]any{
			"units":           `{unit=~"jellyfin.service"}`,
			"minutes_back":    float64(30),
			"around_incident": false,
		},
	); err != nil {
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

func TestParseDiagnosticResult_RoundTripsThroughMapAny(t *testing.T) {
	t.Parallel()
	// db.SetIncidentFinding persists a DiagnosticResult generically as `any`
	// and db.Incident.Finding comes back as map[string]any (via
	// json.Unmarshal into an `any` field) — reproduce that shape rather than
	// passing a *DiagnosticResult directly, which would trivially "round trip".
	want := agent.DiagnosticResult{
		RootCause: "bad release", Confidence: "high",
		PrimaryAction: "manual_investigation", PrimaryReason: "file is corrupt",
		EscalateAction: "remove_and_search", RequiresApproval: true,
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var asMap map[string]any
	if unmarshalErr := json.Unmarshal(b, &asMap); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}

	got, ok := agent.ParseDiagnosticResult(asMap)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.RootCause != want.RootCause || got.EscalateAction != want.EscalateAction {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseDiagnosticResult_NilFinding(t *testing.T) {
	t.Parallel()
	if _, ok := agent.ParseDiagnosticResult(nil); ok {
		t.Error("expected ok=false for a nil finding")
	}
}

func TestSystemPromptStuckReadSection(t *testing.T) {
	t.Parallel()
	const header = "--- Stuck read (what=stuck_read) ---"
	prompt := agent.SystemPromptForTest()
	_, rest, ok := strings.Cut(prompt, header)
	if !ok {
		t.Fatalf("system prompt has no %q section", header)
	}
	section, _, _ := strings.Cut(rest, "\n---")
	for _, want := range []string{"dd_readability_test", "decypharr_recheck", "refresh_decypharr_links", "delete_torrent_readd"} {
		if !strings.Contains(section, want) {
			t.Errorf("stuck_read section does not mention %s", want)
		}
	}
	if strings.Contains(section, "list_directory on its folder") {
		t.Error("stuck_read section calls list_directory safe; it stats every file in the folder")
	}
}

func TestE3_ScanStatusErrorKeepsNote(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	jrnl := journal.New(database)
	ctx := context.Background()
	cs, srv := newCallServer(t)
	cs.scanStatusCode = http.StatusInternalServerError
	const (
		incidentA = "11111111-1111-1111-1111-111111111111"
		incidentB = "22222222-2222-2222-2222-222222222222"
	)
	for _, id := range []string{incidentA, incidentB} {
		lifecycletest.Incident(t, database, &db.Incident{
			ID: id, Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: id,
		})
	}
	a := agent.New(nil, "", fullDispatcher(srv.URL), database, jrnl, slog.New(slog.DiscardHandler))
	if err := jrnl.LogAction(ctx, &db.ActionLog{
		IncidentID: incidentB, Action: "jellyfin_library_scan", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true); err != nil {
		t.Fatal(err)
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note == "" {
		t.Fatal("expected a note while the scan status can't be read")
	}
}

func TestE3_OlderSettleKeepsNewerRestartNote(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	jrnl := journal.New(database)
	ctx := context.Background()
	const (
		incidentA = "11111111-1111-1111-1111-111111111111"
		incidentB = "22222222-2222-2222-2222-222222222222"
	)
	for _, id := range []string{incidentA, incidentB} {
		lifecycletest.Incident(t, database, &db.Incident{
			ID: id, Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: id,
		})
	}
	a := agent.New(nil, "", nil, database, jrnl, slog.New(slog.DiscardHandler))
	restart := func(callID string) {
		t.Helper()
		if err := jrnl.LogActionGen(ctx, &db.ActionLog{
			IncidentID: incidentB, Action: "restart_jellyfin", TriggeredBy: "agent", Status: db.ActionApplied,
		}, true, 1, callID); err != nil {
			t.Fatal(err)
		}
	}
	restart("call-1")
	if err := jrnl.SettleDisruption(ctx, journal.Disruption{IncidentID: incidentB, CallID: "call-1"}); err != nil {
		t.Fatal(err)
	}
	restart("call-2")
	if note := a.DisruptionNoteForTest(ctx, incidentA); note == "" {
		t.Fatal("an earlier restart's settle hid the newer unsettled restart")
	}
}

func disruptionFixture(t *testing.T, srvURL string) (*agent.Agent, *journal.Journal, string, string) {
	t.Helper()
	database := newTestAgentDB(t)
	jrnl := journal.New(database)
	const (
		incidentA = "11111111-1111-1111-1111-111111111111"
		incidentB = "22222222-2222-2222-2222-222222222222"
	)
	for _, id := range []string{incidentA, incidentB} {
		lifecycletest.Incident(t, database, &db.Incident{
			ID: id, Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: id,
		})
	}
	var disp *agent.Dispatcher
	if srvURL != "" {
		disp = fullDispatcher(srvURL)
	}
	return agent.New(nil, "", disp, database, jrnl, slog.New(slog.DiscardHandler)), jrnl, incidentA, incidentB
}

func logDisruptionAgo(t *testing.T, jrnl *journal.Journal, incident, action string, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago)
	if err := jrnl.LogAction(context.Background(), &db.ActionLog{
		IncidentID: incident, Action: action, TriggeredBy: "agent", Status: db.ActionApplied, AppliedAt: &at,
	}, true); err != nil {
		t.Fatal(err)
	}
}

func TestF3_CleanupWarnsTwoMinutes(t *testing.T) {
	t.Parallel()
	a, jrnl, incidentA, incidentB := disruptionFixture(t, "")
	logDisruptionAgo(t, jrnl, incidentB, "decypharr_cache_cleanup", 90*time.Second)
	if note := a.DisruptionNoteForTest(context.Background(), incidentA); note == "" {
		t.Fatal("expected a note 90s after a cache cleanup")
	}
	logDisruptionAgo(t, jrnl, incidentB, "decypharr_cache_cleanup", 3*time.Minute)
	if note := a.DisruptionNoteForTest(context.Background(), incidentA); note != "" {
		t.Fatalf("expected no note 3 minutes after a cache cleanup, got %q", note)
	}
}

func TestF4_SecondRestartSameGenStillWarns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, jrnl, incidentA, incidentB := disruptionFixture(t, "")
	for _, id := range []string{"call-1", "call-2"} {
		if err := jrnl.LogActionGen(ctx, &db.ActionLog{
			IncidentID: incidentB, Action: "restart_jellyfin", TriggeredBy: "agent", Status: db.ActionApplied,
		}, true, 1, id); err != nil {
			t.Fatal(err)
		}
		if id == "call-1" {
			if err := jrnl.SettleDisruption(ctx, journal.Disruption{IncidentID: incidentB, CallID: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if note := a.DisruptionNoteForTest(ctx, incidentA); note == "" {
		t.Fatal("the first restart's settle hid the second restart in the same generation")
	}
}

func TestF5_ScanJustTriggeredStillWarns(t *testing.T) {
	t.Parallel()
	cs, srv := newCallServer(t)
	cs.scanRunning = false
	a, jrnl, incidentA, incidentB := disruptionFixture(t, srv.URL)
	logDisruptionAgo(t, jrnl, incidentB, "jellyfin_library_scan", 20*time.Second)
	if note := a.DisruptionNoteForTest(context.Background(), incidentA); note == "" {
		t.Fatal("expected a note 20s after a scan was triggered, before it could start")
	}
	logDisruptionAgo(t, jrnl, incidentB, "jellyfin_library_scan", 2*time.Minute)
	if note := a.DisruptionNoteForTest(context.Background(), incidentA); note != "" {
		t.Fatalf("expected no note 2 minutes after a scan that is no longer running, got %q", note)
	}
}

func TestG3_LokiFiltersBuildLooseRegex(t *testing.T) {
	t.Parallel()
	got := agent.LokiFilterRegexForTest([]string{"Frieren: Beyond Journey's End", "57ea761f"})
	want := `(?i)(Frieren[^[:alnum:]]*Beyond[^[:alnum:]]*Journey[^[:alnum:]]*s[^[:alnum:]]*End|57ea761f)`
	if got != want {
		t.Fatalf("regex = %q, want %q", got, want)
	}
	if !regexp.MustCompile(got).MatchString("frieren.beyond.journeys.end.s01e01.mkv") {
		t.Errorf("regex %q does not match the release name", got)
	}
	if r := agent.LokiFilterRegexForTest([]string{"...", "A.B", "ab"}); r != "" {
		t.Errorf("unusable terms gave %q, want empty", r)
	}
	if r := agent.LokiFilterRegexForTest(nil); r != "" {
		t.Errorf("nil gave %q, want empty", r)
	}
	r := agent.LokiFilterRegexForTest([]string{"aaa", "bbb", "ccc", "ddd", "eee", "fff"})
	if r != "(?i)(aaa|bbb|ccc|ddd|eee)" {
		t.Errorf("cap of 5 terms: got %q", r)
	}
}

func TestG3_LokiQueryFilterBecomesLineRegex(t *testing.T) {
	t.Parallel()
	loki, got, all := lokiCapturingServer(t)
	disp := &agent.Dispatcher{Loki: loki}

	if _, err := disp.CallScoped(context.Background(), agent.Scope{}, "loki_query", map[string]any{
		"units":        `{unit=~"jellyfin.service"}`,
		"minutes_back": float64(30),
		"filters":      []any{"Abc.Def", `xy"z1`},
		"filter":       "Ghi",
	}); err != nil {
		t.Fatal(err)
	}
	want := `{unit=~"jellyfin.service"} |~ "(?i)(Abc[^[:alnum:]]*Def|xy[^[:alnum:]]*z1|Ghi)"`
	if q := got.Get("query"); q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
	if len(*all) != 1 {
		t.Errorf("queries run = %d, want 1", len(*all))
	}

	loki2, got2, _ := lokiCapturingServer(t)
	disp2 := &agent.Dispatcher{Loki: loki2}
	_, err := disp2.CallScoped(context.Background(), agent.Scope{}, "loki_query", map[string]any{
		"units":        `{unit=~"jellyfin.service"}`,
		"minutes_back": float64(30),
		"filters":      []any{"葬送のフリーレン"},
	})
	if err == nil || !strings.Contains(err.Error(), "no usable filter terms: use the release/file name or item id") {
		t.Errorf("err = %v, want no usable filter terms", err)
	}
	if got2.Get("query") != "" {
		t.Errorf("an unfiltered query ran: %q", got2.Get("query"))
	}
}

func TestG3_LokiSchemaDeclaresFilters(t *testing.T) {
	t.Parallel()
	for _, d := range agent.ToolDefsForTest() {
		if d.Function.Name != "loki_query" {
			continue
		}
		raw, err := json.Marshal(d.Function.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err = json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		var f struct {
			Type  string `json:"type"`
			Items struct {
				Type string `json:"type"`
			} `json:"items"`
		}
		if err = json.Unmarshal(schema.Properties["filters"], &f); err != nil {
			t.Fatal(err)
		}
		if f.Type != "array" || f.Items.Type != "string" {
			t.Errorf("filters schema = %s, want array of string", schema.Properties["filters"])
		}
		return
	}
	t.Fatal("loki_query tool not found")
}
