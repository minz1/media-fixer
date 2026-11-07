package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
	"github.com/minz1/mediafixer/internal/server"
)

// errBoom is a sentinel error used to exercise ApproveEscalation's failure path.
var errBoom = errors.New("boom")

// stubEscalationAgent is a minimal incident.AgentRunner that only needs to
// answer PlanEscalation/RunEscalation meaningfully — the rest of the
// interface is never exercised by the escalation routes under test.
type stubEscalationAgent struct {
	planResult any
	planErr    error
	runResult  any
	runErr     error

	// gotPlanJSON records the bytes ExecuteApprovedPlan was handed, so a test
	// can assert the approval ran the stored preview rather than a
	// re-resolution.
	mu          sync.Mutex
	gotPlanJSON []byte
}

func (a *stubEscalationAgent) Run(
	_ context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage, _ agent.Gate,
) agent.RunExit {
	return exitOf(nil, nil, nil)
}

func (a *stubEscalationAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	return true
}
func (a *stubEscalationAgent) ScanRunning(_ context.Context) bool { return false }

func (a *stubEscalationAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *stubEscalationAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult, _ string) (any, error) {
	return a.planResult, a.planErr
}

func (a *stubEscalationAgent) ExecuteApprovedPlan(
	_ context.Context, _ *agent.DiagnosticResult, _ []byte,
) (any, error) {
	return a.runResult, a.runErr
}

func (a *stubEscalationAgent) approvedPlanJSON() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gotPlanJSON
}

func (a *stubEscalationAgent) CheckPendingOutcome(
	_ context.Context, _ *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return &agent.PendingOutcomeObservation{}, nil
}

// newEscalationTestServer builds a server backed by a real DB and a stub
// agent, so the escalation routes exercise the real incident.Service and
// db.DB code paths end to end, with only the LLM-facing plan/run calls faked.
func newEscalationTestServer(t *testing.T, ag incident.AgentRunner) (*server.Server, *db.DB) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	discard := slog.New(slog.DiscardHandler)
	notif := &stubNotifier{}
	// Server and Service must share one Journal instance: Subscribe/notify
	// state lives in-process on the Journal value itself, not in the
	// database, so two separately-constructed journal.New(database) values
	// would never see each other's appends.
	jrnl := journal.New(database)
	svc := incident.NewService(context.Background(), database, jrnl, ag, nil, nil, notif, discard)
	if stub, ok := ag.(*stubEscalationAgent); ok {
		svc.SetOpPorts(opexec.Ports{Arr: func(string) opexec.Arr { return stubArr{ag: stub} }})
	}
	t.Cleanup(func() { stopService(t, svc) })
	srv, err := server.New(":0", "/media", database, jrnl, svc, discard)
	if err != nil {
		t.Fatal(err)
	}
	return srv, database
}

// newManualTestNeededIncident creates an incident already sitting in
// manual_test_needed with a remove_and_search finding, as if the agent had
// already diagnosed and escalated it.
func newManualTestNeededIncident(t *testing.T, database *db.DB) *db.Incident {
	t.Helper()
	return newManualTestNeededIncidentWhat(t, database, "cant_play")
}

func newManualTestNeededIncidentWhat(t *testing.T, database *db.DB, what string) *db.Incident {
	t.Helper()
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: what, Title: "The Boys"}
	lifecycletest.Incident(t, database, inc)
	finding := agent.DiagnosticResult{
		RootCause:        "bad release",
		Confidence:       "high",
		PrimaryAction:    "manual_investigation",
		PrimaryReason:    "file is corrupt",
		EscalateAction:   agent.EscalateRemoveAndSearch,
		RequiresApproval: true,
		EscalateParams: map[string]any{
			"media_type": "tv",
			"title":      "The Boys",
			"scope":      "episode",
			"season":     float64(1),
			"episode":    float64(1),
			"blocklist":  true,
		},
	}
	lifecycletest.Finding(t, database, inc.ID, finding)
	lifecycletest.MoveTo(t, database, inc.ID, db.StatusManualTestNeeded)
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// waitForStatus polls until the incident reaches want or a short deadline
// expires, returning whatever the last read was either way.
func waitForStatus(t *testing.T, database *db.DB, id string, want db.IncidentStatus) *db.Incident {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got *db.Incident
	for time.Now().Before(deadline) {
		inc, err := database.GetIncident(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		got = inc
		if inc.Status == want {
			return inc
		}
		time.Sleep(20 * time.Millisecond)
	}
	return got
}

func storedPlanFP(t *testing.T, database *db.DB, id string) string {
	t.Helper()
	plan, _, err := database.GetEscalationPlan(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return card.Fingerprint(plan)
}

func TestEscalationPreview_Success(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{planResult: map[string]any{"would_delete": []string{"ep01.mkv"}}}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().
		Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview?v=" + curV(t, database, inc.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "would_delete") {
		t.Errorf("preview body missing plan content: %s", body)
	}
}

func TestEscalationPreview_NoFinding(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{}
	srv, database := newEscalationTestServer(t, ag)

	inc := &db.Incident{
		Status: db.StatusManualTestNeeded, Source: "discord", What: "cant_play", Title: "No Finding Show",
	}
	lifecycletest.Incident(t, database, inc)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().
		Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview?v=" + curV(t, database, inc.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// htmx does not swap non-2xx responses, so an unresolvable preview must
	// still render 200 with the error inline.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "no diagnostic finding") {
		t.Errorf("expected inline error, got: %s", body)
	}
}

func TestApproveEscalation_Success(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{
		planResult: map[string]any{"MediaType": "tv", "Title": "T"},
		runResult:  map[string]any{"deleted_files": []string{"ep01.mkv"}},
	}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Approval executes the stored preview, so take one first.
	previewResp, err := ts.Client().
		Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview?v=" + curV(t, database, inc.ID))
	if err != nil {
		t.Fatal(err)
	}
	previewResp.Body.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation",
		url.Values{"plan_fp": {storedPlanFP(t, database, inc.ID)}, "v": {curV(t, database, inc.ID)}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// The post-approval verification loop transitions the incident to
	// "verifying" from its own background goroutine — poll briefly instead
	// of asserting immediately after the HTTP response.
	got := waitForStatus(t, database, inc.ID, db.StatusVerifying)
	if got.Status != db.StatusVerifying {
		t.Errorf("status = %q, want %q", got.Status, db.StatusVerifying)
	}

	actions, err := database.ListActions(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].TriggeredBy != "owner" || actions[0].Action != agent.EscalateRemoveAndSearch {
		t.Errorf("actions = %+v", actions)
	}
}

func TestApproveEscalation_RunFails(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{runErr: errBoom}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Approval now requires a stored preview, so take one first — this test
	// is about the run failing, not about the missing-plan guard.
	previewResp, err := ts.Client().
		Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview?v=" + curV(t, database, inc.ID))
	if err != nil {
		t.Fatal(err)
	}
	previewResp.Body.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation",
		url.Values{"plan_fp": {storedPlanFP(t, database, inc.ID)}, "v": {curV(t, database, inc.ID)}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	got, err := database.GetIncident(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusManualTestNeeded {
		t.Errorf("status = %q, want unchanged %q", got.Status, db.StatusManualTestNeeded)
	}

	actions, err := database.ListActions(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Status != db.ActionFailed {
		t.Errorf("actions = %+v", actions)
	}
}

// TestApproveEscalation_RequiresAPreview pins the approval contract: a
// remove-and-search deletes files, and the only confirmation is the preview,
// so approving without one has nothing to be a confirmation of.
func TestApproveEscalation_RequiresAPreview(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{planResult: map[string]any{"Files": []any{}}}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusSeeOther {
		t.Error("approval succeeded with no previewed plan; the owner confirmed nothing")
	}
	if ag.approvedPlanJSON() != nil {
		t.Error("an escalation was executed without a stored plan")
	}
}

// TestApproveEscalation_RunsThePreviewedPlan is the TOCTOU regression test:
// approval must execute the bytes the preview stored, not whatever a fresh
// resolution would return afterwards. The stub deliberately changes what
// PlanEscalation returns between the preview and the approval, standing in
// for the library moving underneath.
func TestApproveEscalation_RunsThePreviewedPlan(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{
		planResult: map[string]any{"MediaType": "tv", "Title": "Previewed"},
		runResult:  map[string]any{"ok": true},
	}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	previewResp, err := ts.Client().
		Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview?v=" + curV(t, database, inc.ID))
	if err != nil {
		t.Fatal(err)
	}
	previewResp.Body.Close()

	// The world moves on between preview and approval.
	ag.planResult = map[string]any{"MediaType": "tv", "Title": "SomethingElse"}

	resp, err := ts.Client().PostForm(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation",
		url.Values{"plan_fp": {storedPlanFP(t, database, inc.ID)}, "v": {curV(t, database, inc.ID)}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := string(ag.approvedPlanJSON())
	if !strings.Contains(got, "Previewed") {
		t.Errorf("executed plan = %s; want the plan the owner was shown", got)
	}
	if strings.Contains(got, "SomethingElse") {
		t.Error("executed a re-resolved plan instead of the approved one")
	}
}

func TestApproveEscalation_SurvivesCancelledRequest(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{
		planResult: map[string]any{"MediaType": "tv", "Title": "T"},
		runResult:  map[string]any{"deleted_files": []string{"ep01.mkv"}},
	}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	preview := httptest.NewRecorder()
	srv.Handler().ServeHTTP(preview, httptest.NewRequest(
		http.MethodGet, "/media/incidents/"+inc.ID+"/escalation-preview?v="+curV(t, database, inc.ID), nil,
	))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status = %d", preview.Code)
	}

	rec := serveCancelledForm(t, srv, "/media/incidents/"+inc.ID+"/approve-escalation",
		url.Values{"plan_fp": {storedPlanFP(t, database, inc.ID)}, "v": {curV(t, database, inc.ID)}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got status %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	got := waitForStatus(t, database, inc.ID, db.StatusVerifying)
	if got.Status != db.StatusVerifying {
		t.Errorf("status = %q, want %q", got.Status, db.StatusVerifying)
	}
	actions, err := database.ListActions(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Action != agent.EscalateRemoveAndSearch {
		t.Errorf("actions = %+v", actions)
	}
}

func TestEscalationPreview_RendersApproveWithPlanFP(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{planResult: map[string]any{"Title": "T"}}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/media/incidents/"+inc.ID+"/escalation-preview?v="+curV(t, database, inc.ID), nil,
	))
	body := rec.Body.String()
	for _, want := range []string{
		"/media/incidents/" + inc.ID + "/approve-escalation", "hx-confirm", `name="plan_fp"`,
		`value="` + storedPlanFP(t, database, inc.ID) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview partial missing %q:\n%s", want, body)
		}
	}
}

func TestIncidentPage_NoApproveBeforePreview(t *testing.T) {
	t.Parallel()
	srv, database := newEscalationTestServer(t, &stubEscalationAgent{})
	inc := newManualTestNeededIncident(t, database)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID, nil))
	body := rec.Body.String()
	if !strings.Contains(body, "escalation-preview") {
		t.Fatalf("incident page has no Preview button:\n%s", body)
	}
	if strings.Contains(body, "approve-escalation") {
		t.Error("incident page offers Approve before any preview")
	}
}

func TestApproveEscalation_MissingPlanFPRefused(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{planResult: map[string]any{"Title": "T"}}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	previewResp, err := ts.Client().
		Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview?v=" + curV(t, database, inc.ID))
	if err != nil {
		t.Fatal(err)
	}
	previewResp.Body.Close()

	noFollow := ts.Client()
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Post(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "?done=refused-plan-changed") {
		t.Errorf("status = %d %s, want a refusal notice", resp.StatusCode, loc)
	}
	if ag.approvedPlanJSON() != nil {
		t.Error("an escalation ran without the previewed plan's fingerprint")
	}
}

func TestResolve_AlreadyResolvedRedirects(t *testing.T) {
	t.Parallel()
	srv, database := newEscalationTestServer(t, &stubEscalationAgent{})
	inc := newManualTestNeededIncident(t, database)
	lifecycletest.MoveTo(t, database, inc.ID, db.StatusResolved)

	rec := serveCancelled(t, srv, "/media/incidents/"+inc.ID+"/resolve")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/media/incidents/"+inc.ID+"?done=resolve" {
		t.Errorf("Location = %q, want the incident page", loc)
	}
}

func TestEscalationPreview_StuckReadNotProbed(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{
		planResult: &client.ReplacePlan{Title: "The Boys", Readability: &client.ReadabilityProbe{}},
	}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncidentWhat(t, database, agent.WhatStuckRead)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/media/incidents/"+inc.ID+"/escalation-preview?v="+curV(t, database, inc.ID), nil,
	))
	body := rec.Body.String()
	if !strings.Contains(body, "Not probed: reading a stuck file would hang") ||
		strings.Contains(body, "no media-agent configured") {
		t.Errorf("stuck_read preview readability line wrong:\n%s", body)
	}
}

func curV(t *testing.T, database *db.DB, id string) string {
	t.Helper()
	s, err := database.GetIncidentSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return "v" + strconv.FormatInt(s.Version, 10)
}

func seedNoRelease(t *testing.T, database *db.DB, title string) *db.Incident {
	t.Helper()
	s := lifecycletest.Seed(t, database, lifecycletest.Spec{
		Title: title, State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	inc, err := database.GetIncident(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func TestEscalationPreview_SurvivesStreamFrame(t *testing.T) {
	t.Parallel()
	assertPreviewSurvivesStreamFrame(t, map[string]any{"MediaType": "tv", "Title": "T"}, "")
}

func TestEscalationPreview_ReadableWarningSurvivesStreamFrame(t *testing.T) {
	t.Parallel()
	readable := "/tv/T/Season 01/T.S01E01.mkv"
	assertPreviewSurvivesStreamFrame(t, &client.ReplacePlan{
		MediaType: "tv", Title: "T",
		Readability: &client.ReadabilityProbe{Checked: true, Probed: 1, ReadableFile: readable},
	}, readable)
}

func assertPreviewSurvivesStreamFrame(t *testing.T, plan any, mustShow string) {
	t.Helper()
	ag := &stubEscalationAgent{
		planResult: plan,
		runResult:  map[string]any{"deleted_files": []string{"ep01.mkv"}},
	}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	streamCtx, cancelStream := context.WithCancel(context.Background())
	t.Cleanup(cancelStream)
	events := openSSEContext(streamCtx, t, ts, "/media/incidents/"+inc.ID+"/events")
	if _, ok := nextSSE(t, events, sseEventDeadline); !ok {
		t.Fatal("no first stream frame")
	}
	previewResp, err := ts.Client().
		Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview?v=" + curV(t, database, inc.ID))
	if err != nil {
		t.Fatal(err)
	}
	previewHTML, readErr := io.ReadAll(previewResp.Body)
	previewResp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if previewResp.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d, body = %s", previewResp.StatusCode, previewHTML)
	}
	v, fp := curV(t, database, inc.ID), storedPlanFP(t, database, inc.ID)
	frame, ok := nextSSE(t, events, sseEventDeadline)
	if !ok {
		t.Fatal("no stream frame after the preview")
	}
	streamHTML := strings.ReplaceAll(strings.TrimPrefix(frame, "data: "), "\ndata: ", "\n")
	if !strings.Contains(streamHTML, string(previewHTML)) {
		t.Fatalf("stream frame did not preserve the Preview response:\n%s", frame)
	}
	if !strings.Contains(string(previewHTML), mustShow) || !strings.Contains(streamHTML, mustShow) {
		t.Fatalf("readable-file warning %q missing from the preview or the stream frame:\n%s", mustShow, frame)
	}
	for _, want := range []string{
		"/media/incidents/" + inc.ID + "/approve-escalation", `name="plan_fp" value="` + fp + `"`,
		`name="v" value="` + v + `"`,
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("stream frame after the preview lost %q:\n%s", want, frame)
		}
	}
	resp, err := ts.Client().PostForm(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation",
		url.Values{"plan_fp": {fp}, "v": {v}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || ag.approvedPlanJSON() == nil {
		t.Fatalf("approve from the streamed form: status %d, ran %v", resp.StatusCode, ag.approvedPlanJSON() != nil)
	}
	waitForStatus(t, database, inc.ID, db.StatusVerifying)
}

func (a *stubEscalationAgent) OperationFor(
	subject operation.Subject, _ *agent.DiagnosticResult, planJSON []byte,
) (operation.Spec, error) {
	a.mu.Lock()
	a.gotPlanJSON = planJSON
	a.mu.Unlock()
	return operation.Spec{
		Subject: subject, Kind: operation.KindReplace, Title: "t", Source: planJSON,
		Steps: []operation.Step{{
			Kind: operation.StepSearch, App: "sonarr", MediaID: 1, Command: "SeasonSearch", Season: 1,
		}},
	}, nil
}

type stubArr struct {
	opexec.Arr

	ag *stubEscalationAgent
}

func (a stubArr) Search(ctx context.Context, _ string, _, _ int, _ []int) error {
	if _, err := a.ag.ExecuteApprovedPlan(ctx, nil, nil); err != nil {
		return &client.HTTPStatusError{Status: http.StatusBadRequest, Body: err.Error()}
	}
	return nil
}
