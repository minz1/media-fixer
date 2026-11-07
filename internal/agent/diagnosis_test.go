package agent_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
)

const (
	searchEpisode9 = `{"media_type":"tv","title":"Rick and Morty","season":9,"episode":9}`
	concludeSearch = `{"root_cause":"missing","confidence":"high","primary_action":"arr_search_missing",` +
		`"primary_reason":"not on disk","requires_approval":false}`
)

func rmEp(ep int) agent.SearchTarget {
	return agent.SearchTarget{MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: ep}
}

func diag(ra bool, ea string, vas int, pa string) *agent.DiagnosticResult {
	return &agent.DiagnosticResult{
		RootCause: "rc", Confidence: "high", PrimaryAction: pa, PrimaryReason: "because",
		EscalateAction: ea, RequiresApproval: ra, VerifyAfterSeconds: vas,
	}
}

func withParams(r *agent.DiagnosticResult, p map[string]any) *agent.DiagnosticResult {
	r.EscalateParams = p
	return r
}

func refusedSearch(t agent.SearchTarget, reason string) agent.CallRecord {
	rec := agent.SearchCalls(t)[0]
	rec.Status = agent.OutcomeRefused
	rec.Err = reason
	rec.Counted = false
	return rec
}

func TestClassify_Table(t *testing.T) {
	t.Parallel()
	e07, e09 := rmEp(7), rmEp(9)
	const search = agent.ToolArrSearchMissing
	ras := map[string]any{"media_type": "tv", "title": "X", "scope": "episode"}
	cases := []struct {
		row     string
		r       *agent.DiagnosticResult
		calls   []agent.CallRecord
		want    agent.DiagnosisOutcome
		wantErr string
	}{
		{"K1", diag(false, "nuke_it", 0, "x"), nil, agent.DiagnosisOutcome{}, "escalate_action must be one of"},
		{"K1 with RA", diag(true, "nuke_it", 0, "x"), nil, agent.DiagnosisOutcome{}, "escalate_action must be one of"},
		{"K2", diag(false, "", -5, "x"), nil, agent.DiagnosisOutcome{}, "verify_after_seconds must be ≥ 0"},
		{
			"K3", diag(true, agent.EscalateManualInvestigation, 60, "x"), nil, agent.DiagnosisOutcome{},
			"approval and deferred verification are exclusive",
		},
		{
			"K4", diag(true, agent.EscalateNone, 0, search), agent.SearchCalls(e09), agent.DiagnosisOutcome{},
			"approval and download tracking are exclusive",
		},
		{
			"K5", diag(false, "", 60, search), agent.SearchCalls(e09), agent.DiagnosisOutcome{},
			"download tracking and deferred verification are exclusive",
		},
		{
			"K6 no record", diag(false, "", 0, search), nil, agent.DiagnosisOutcome{},
			"arr_search_missing was not applied in this run",
		},
		{
			"K6 refused", diag(false, "", 0, search), []agent.CallRecord{refusedSearch(e09, "has a file")},
			agent.DiagnosisOutcome{}, "arr_search_missing was not applied in this run: refused: has a file",
		},
		{
			"K7", diag(false, agent.EscalateRemoveAndSearch, 0, search), agent.SearchCalls(e07, e09),
			agent.DiagnosisOutcome{
				Kind: agent.DiagnosisAwaitDownload, Target: &e09, SentNotTracked: []agent.SearchTarget{e07},
			}, "",
		},
		{
			"K8", diag(false, "", 120, "sonarr_rescan"), nil,
			agent.DiagnosisOutcome{Kind: agent.DiagnosisDeferredVerify, AfterSeconds: 120}, "",
		},
		{"K9", diag(false, "", 0, "restart_jellyfin"), nil, agent.DiagnosisOutcome{Kind: agent.DiagnosisFixNow}, ""},
		{
			"K9 search on record, other action", diag(false, "", 0, "restart_jellyfin"), agent.SearchCalls(e09),
			agent.DiagnosisOutcome{Kind: agent.DiagnosisFixNow}, "",
		},
		{
			"K10 remove_and_search", diag(true, agent.EscalateRemoveAndSearch, 0, "x"), nil, agent.DiagnosisOutcome{},
			"escalate_params is required for remove_and_search",
		},
		{
			"K10 delete_torrent_readd", withParams(diag(true, agent.EscalateDeleteTorrentReadd, 0, "x"),
				map[string]any{"hash": "h"}), nil, agent.DiagnosisOutcome{},
			"escalate_params.name is required for delete_torrent_readd",
		},
		{
			"K11 remove_and_search", withParams(diag(true, agent.EscalateRemoveAndSearch, 0, "x"), ras), nil,
			agent.DiagnosisOutcome{Kind: agent.DiagnosisNeedsApproval, Plan: agent.EscalateRemoveAndSearch}, "",
		},
		{
			"K11 delete_torrent_readd", withParams(diag(true, agent.EscalateDeleteTorrentReadd, 0, "x"),
				map[string]any{"name": "Rick.S09"}), nil,
			agent.DiagnosisOutcome{Kind: agent.DiagnosisNeedsApproval, Plan: agent.EscalateDeleteTorrentReadd}, "",
		},
		{
			"K12 manual", diag(true, agent.EscalateManualInvestigation, 0, "x"), nil,
			agent.DiagnosisOutcome{Kind: agent.DiagnosisManual, Reason: "because"}, "",
		},
		{
			"K12 none", diag(true, agent.EscalateNone, 0, "x"), nil,
			agent.DiagnosisOutcome{Kind: agent.DiagnosisManual, Reason: "because"}, "",
		},
		{
			"K12 empty", diag(true, "", 0, "x"), nil,
			agent.DiagnosisOutcome{Kind: agent.DiagnosisManual, Reason: "because"}, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			got, err := agent.Classify(tc.r, tc.calls)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("outcome = %+v, want %+v", got, tc.want)
			}
			tc.r.Outcome = got
			if checkErr := agent.CheckDiagnosis(tc.r); checkErr != nil {
				t.Errorf("CheckDiagnosis: %v", checkErr)
			}
		})
	}
}

func TestClassify_ContradictionsAreToolErrors(t *testing.T) {
	t.Parallel()
	cases := []struct{ row, args, want string }{
		{"E1", `{"root_cause":"x","primary_action":"x","escalate_action":"nuke","requires_approval":true}`,
			"escalate_action must be one of"},
		{"E2", `{"root_cause":"x","primary_action":"x","verify_after_seconds":-1}`, "verify_after_seconds must be"},
		{"E3", `{"root_cause":"x","primary_action":"x","requires_approval":true,"verify_after_seconds":60}`,
			"approval and deferred verification are exclusive"},
		{"E4", `{"root_cause":"x","primary_action":"arr_search_missing","requires_approval":true}`,
			"approval and download tracking are exclusive"},
		{"E5", `{"root_cause":"x","primary_action":"arr_search_missing","verify_after_seconds":60}`,
			"download tracking and deferred verification are exclusive"},
		{"E6", concludeSearch, "arr_search_missing was not applied in this run"},
		{"E7", `{"root_cause":"x","primary_action":"x","requires_approval":true,"escalate_action":"remove_and_search"}`,
			"escalate_params is required for remove_and_search"},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			t.Parallel()
			_, srv := newCallServer(t)
			ag, database, inc := newRig(t, srv.URL, []string{
				toolCallResponse("complete_diagnosis", tc.args),
				toolCallResponse("complete_diagnosis", diagnosisArgs),
			}, rigOpts{})
			exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
			if exit.State != agent.RunConcluded {
				t.Fatalf("exit %s: %v, want the run to go on and conclude", exit.State, exit.Err)
			}
			replies := toolReplies(exit.Conversation)
			if len(replies) != 2 || !strings.Contains(replies[0], tc.want) {
				t.Fatalf("replies = %v, want the first to be the tool error %q", replies, tc.want)
			}
			if exit.Finding == nil || exit.Finding.Outcome.Kind != agent.DiagnosisManual {
				t.Errorf("finding = %+v, want the second, valid conclusion", exit.Finding)
			}
		})
	}
}

func TestClassify_MalformedArgsIsToolError(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("complete_diagnosis", `{"root_cause":`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v, want a tool error and a later conclusion, not batch_failed", exit.State, exit.Err)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) != 2 || !strings.Contains(replies[0], `"error"`) {
		t.Fatalf("replies = %v, want an error reply to the malformed call", replies)
	}
}

func TestA7_7_NoSearchThisRunIsToolError(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.episodeHasFile = true
	ag, database, inc := newRig(t, srv.URL, []string{
		batchResponse(
			call{"arr_search_missing", searchEpisode7},
			call{"complete_diagnosis", concludeSearch},
			call{"get_disk_info", `{}`},
		),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v, want the next round to conclude", exit.State, exit.Err)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) != 4 {
		t.Fatalf("replies = %v, want one per call in the batch plus the conclusion", replies)
	}
	want := "arr_search_missing was not applied in this run: refused: " + agent.ErrArrTargetHasFile.Error()
	if !strings.Contains(replies[1], want) {
		t.Errorf("conclusion reply = %q, want E6 with the refusal %q", replies[1], want)
	}
	if strings.Contains(replies[2], "not run") {
		t.Errorf("later call reply = %q, want the batch to continue and run it", replies[2])
	}
}

func TestA7_7_AwaitDownloadTargetFromThisRun(t *testing.T) {
	t.Parallel()
	e09 := rmEp(9)
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", searchEpisode9),
		toolCallResponse("complete_diagnosis", concludeSearch),
	}, rigOpts{})
	old := &db.ActionLog{
		IncidentID: inc.ID, Action: agent.ToolArrSearchMissing, TriggeredBy: "agent", Status: db.ActionApplied,
		Params: map[string]any{"media_type": "tv", "title": "Rick and Morty", "season": 9, "episode": 7},
	}
	if err := journal.New(database).LogActionGen(context.Background(), old, false, inc.Generation-1); err != nil {
		t.Fatal(err)
	}
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded || exit.Finding == nil {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	o := exit.Finding.Outcome
	if o.Kind != agent.DiagnosisAwaitDownload || o.Target == nil || *o.Target != e09 {
		t.Fatalf("outcome = %+v, want AwaitDownload on this run's S09E09", o)
	}
	if len(o.SentNotTracked) != 0 {
		t.Errorf("sent_not_tracked = %v, want the older generation's search left out", o.SentNotTracked)
	}
}

func TestClassify_LastSearchWinsEarlierSentNotTracked(t *testing.T) {
	t.Parallel()
	e07, e09 := rmEp(7), rmEp(9)
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", searchEpisode7),
		toolCallResponse("arr_search_missing", searchEpisode9),
		toolCallResponse("complete_diagnosis", concludeSearch),
	}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded || exit.Finding == nil {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	want := agent.DiagnosisOutcome{
		Kind: agent.DiagnosisAwaitDownload, Target: &e09, SentNotTracked: []agent.SearchTarget{e07},
	}
	if !reflect.DeepEqual(exit.Finding.Outcome, want) {
		t.Fatalf("outcome = %+v, want %+v", exit.Finding.Outcome, want)
	}
	stored, err := database.GetIncident(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := agent.ParseDiagnosticResult(stored.Finding)
	if !ok || !reflect.DeepEqual(parsed.Outcome, want) {
		t.Errorf("stored finding outcome = %+v, want %+v", parsed, want)
	}
	events, err := database.EventsSince(context.Background(), inc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var journalled *agent.DiagnosticResult
	for _, ev := range events {
		if ev.Kind == string(journal.KindDiagnosisCompleted) {
			journalled = &agent.DiagnosticResult{}
			if uerr := json.Unmarshal(ev.Payload, journalled); uerr != nil {
				t.Fatal(uerr)
			}
		}
	}
	if journalled == nil || !reflect.DeepEqual(journalled.Outcome, want) {
		t.Errorf("journalled diagnosis = %+v, want outcome %+v", journalled, want)
	}
}

func TestClassify_RetryInheritsSearch(t *testing.T) {
	t.Parallel()
	e09 := rmEp(9)
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", searchEpisode9),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
		toolCallResponse("complete_diagnosis", concludeSearch),
	}, rigOpts{})
	g := newRecGate(database, inc)
	first := runGated(context.Background(), t, ag, database, inc, g)
	if first.State != agent.RunConcluded {
		t.Fatalf("attempt 1 exit %s: %v", first.State, first.Err)
	}
	second := runGated(context.Background(), t, ag, database, inc, g)
	if second.State != agent.RunConcluded || second.Finding == nil {
		t.Fatalf("attempt 2 exit %s: %v", second.State, second.Err)
	}
	o := second.Finding.Outcome
	if o.Kind != agent.DiagnosisAwaitDownload || o.Target == nil || *o.Target != e09 {
		t.Errorf("outcome = %+v, want attempt 1's search as the target", o)
	}
}

func TestDiagnosis_VerifyFailureBecomesManual(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	fix := `{"root_cause":"x","confidence":"high","primary_action":"refresh_decypharr_links",` +
		`"primary_reason":"links stale","requires_approval":false}`
	ag, database, inc := newRig(t, srv.URL, []string{toolCallResponse("complete_diagnosis", fix)}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded || exit.Finding == nil {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	f := exit.Finding
	wantReason := "autonomous fix applied but playback verification failed; links stale"
	if f.Outcome.Kind != agent.DiagnosisManual || f.Outcome.Reason != wantReason {
		t.Errorf("outcome = %+v, want Manual %q", f.Outcome, wantReason)
	}
	if !f.RequiresApproval || f.EscalateAction != agent.EscalateManualInvestigation {
		t.Errorf("legacy flags = RA %v EA %q, want the manual rewrite", f.RequiresApproval, f.EscalateAction)
	}
	if err := agent.CheckDiagnosis(f); err != nil {
		t.Errorf("CheckDiagnosis: %v", err)
	}
}

func TestDiagnosis_RunExitsAreManual(t *testing.T) {
	t.Parallel()
	n := 0
	for _, s := range agent.RunStates() {
		if !agent.RunMeta(s).WritesFinding {
			continue
		}
		n++
		f := agent.ManualFindingForTest(s, nil)
		if f.Outcome.Kind != agent.DiagnosisManual || f.Outcome.Reason != agent.RunMeta(s).Reason {
			t.Errorf("%s: outcome = %+v, want Manual{%q}", s, f.Outcome, agent.RunMeta(s).Reason)
		}
		if err := agent.CheckDiagnosis(f); err != nil {
			t.Errorf("%s: CheckDiagnosis: %v", s, err)
		}
	}
	if n == 0 {
		t.Fatal("no WritesFinding state")
	}
}

func TestDiagnosis_StoredJSONKeepsLegacyFlags(t *testing.T) {
	t.Parallel()
	r := withParams(diag(true, agent.EscalateRemoveAndSearch, 0, "x"), map[string]any{"title": "X"})
	before, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if uerr := json.Unmarshal(before, &legacy); uerr != nil {
		t.Fatal(uerr)
	}
	if _, has := legacy["outcome"]; has {
		t.Fatalf("unclassified result marshals an outcome key: %s", before)
	}
	o, err := agent.Classify(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Outcome = o
	after, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if uerr := json.Unmarshal(after, &got); uerr != nil {
		t.Fatal(uerr)
	}
	outcome, has := got["outcome"].(map[string]any)
	if !has || outcome["kind"] != string(agent.DiagnosisNeedsApproval) ||
		outcome["plan"] != agent.EscalateRemoveAndSearch {
		t.Errorf("outcome = %v, want needs_approval with its plan", got["outcome"])
	}
	delete(got, "outcome")
	if !reflect.DeepEqual(got, legacy) {
		t.Errorf("legacy keys changed:\n got %v\nwant %v", got, legacy)
	}
}

func TestLunaP2_SupersededConclusionNotJournalled(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	var g *recGate
	ag, database, inc := newRig(t, srv.URL, []string{toolCallResponse("complete_diagnosis", diagnosisArgs)},
		rigOpts{onLLM: func() { g.stale.Store(true) }})
	g = newRecGate(database, inc)
	exit := ag.Run(context.Background(), inc, nil, g)
	if exit.State != agent.RunSuperseded {
		t.Fatalf("exit %s, want superseded", exit.State)
	}
	events, err := database.EventsSince(context.Background(), inc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == string(journal.KindDiagnosisCompleted) {
			t.Fatalf("a superseded run journalled diagnosis_completed: %s", ev.Payload)
		}
	}
}

func FuzzClassify(f *testing.F) {
	f.Add(false, "", 0, "restart_jellyfin", false, uint8(0), "")
	f.Add(true, agent.EscalateRemoveAndSearch, 0, "x", true, uint8(2), "title")
	f.Add(false, "", 0, agent.ToolArrSearchMissing, true, uint8(3), "")
	f.Add(true, agent.EscalateDeleteTorrentReadd, 30, agent.ToolArrSearchMissing, false, uint8(1), "name")
	statuses := []agent.CallOutcome{
		agent.OutcomeApplied, agent.OutcomeFailed, agent.OutcomeRefused, agent.OutcomeInterrupted, agent.OutcomeInvalid,
	}
	f.Fuzz(func(t *testing.T, ra bool, ea string, vas int, pa string, withCalls bool, mask uint8, key string) {
		r := diag(ra, ea, vas, pa)
		if key != "" {
			r.EscalateParams = map[string]any{key: "v"}
		}
		var calls []agent.CallRecord
		if withCalls {
			for i, st := range statuses {
				if mask&(1<<i) == 0 {
					continue
				}
				rec := agent.SearchCalls(agent.SearchTarget{MediaType: "tv", Title: "T", Season: i, Episode: i})[0]
				rec.Status = st
				calls = append(calls, rec)
			}
		}
		o, err := agent.Classify(r, calls)
		if err != nil {
			return
		}
		r.Outcome = o
		if checkErr := agent.CheckDiagnosis(r); checkErr != nil {
			t.Fatalf("Classify(%+v) = %+v, CheckDiagnosis: %v", r, o, checkErr)
		}
	})
}

const contradiction = `{"root_cause":"rc","confidence":"medium","primary_action":"x","requires_approval":true,` +
	`"verify_after_seconds":60}`

func assertCapped(t *testing.T, exit agent.RunExit) {
	t.Helper()
	if exit.State != agent.RunConcluded || exit.Finding == nil {
		t.Fatalf("exit %s: %v, want concluded", exit.State, exit.Err)
	}
	f := exit.Finding
	want := agent.DiagnosisOutcome{Kind: agent.DiagnosisManual, Reason: "the diagnosis contradicted itself"}
	if !reflect.DeepEqual(f.Outcome, want) {
		t.Errorf("outcome = %+v, want %+v", f.Outcome, want)
	}
	if !f.RequiresApproval || f.EscalateAction != agent.EscalateManualInvestigation || f.EscalateParams != nil {
		t.Errorf("flags = RA %v EA %q params %v, want manual with no params", f.RequiresApproval, f.EscalateAction,
			f.EscalateParams)
	}
	if f.PrimaryReason != want.Reason || f.RootCause != "rc" || f.Confidence != "medium" {
		t.Errorf("finding = %+v, want the reason set and root cause and confidence kept", f)
	}
	if err := agent.CheckDiagnosis(f); err != nil {
		t.Errorf("CheckDiagnosis: %v", err)
	}
}

func TestRulingA1_ThirdDiagnosisErrorEndsManual(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	assertCapped(t, exit)
	replies := toolReplies(exit.Conversation)
	if len(replies) != 3 {
		t.Fatalf("replies = %v, want three, the run ending on the third", replies)
	}
	for i, r := range replies[:2] {
		if !strings.Contains(r, "approval and deferred verification are exclusive") {
			t.Errorf("reply %d = %q, want the tool error", i, r)
		}
	}
}

func TestRulingA1_CapSpansRetryAttempts(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
		toolCallResponse("complete_diagnosis", `{"root_cause":`),
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	g := newRecGate(database, inc)
	first := runGated(context.Background(), t, ag, database, inc, g)
	if first.State != agent.RunConcluded || first.Finding.Outcome.Reason == "the diagnosis contradicted itself" {
		t.Fatalf("attempt 1 = %s %+v, want its own conclusion", first.State, first.Finding)
	}
	assertCapped(t, runGated(context.Background(), t, ag, database, inc, g))
}

func TestRulingA1_CapResetsOnNewGeneration(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
		toolCallResponse("complete_diagnosis", contradiction),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	first := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if first.State != agent.RunConcluded {
		t.Fatalf("generation 1 exit %s: %v", first.State, first.Err)
	}
	lifecycletest.Force(t, database, inc.ID, func(s *lifecycle.Snapshot) {
		s.Generation, s.Status = inc.Generation+1, lifecycle.Open
	})
	next := newRecGate(database, inc)
	next.gen = inc.Generation + 1
	second := ag.Run(context.Background(), inc, nil, next)
	if second.Finding == nil || second.Finding.Outcome.Reason == "the diagnosis contradicted itself" {
		t.Fatalf("generation 2 = %s %+v, want a fresh cap", second.State, second.Finding)
	}
}
