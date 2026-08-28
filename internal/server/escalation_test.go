package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
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
}

func (a *stubEscalationAgent) Run(
	_ context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	return nil, nil, nil
}

func (a *stubEscalationAgent) VerifyResolved(_ context.Context, _ string, _ *agent.FixSignature) bool {
	return true
}
func (a *stubEscalationAgent) ScanRunning(_ context.Context) bool { return false }

func (a *stubEscalationAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *stubEscalationAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return a.planResult, a.planErr
}

func (a *stubEscalationAgent) RunEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return a.runResult, a.runErr
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
	svc := incident.NewService(context.Background(), database, ag, nil, nil, notif, discard)
	srv, err := server.New(":0", "/media", database, svc, discard)
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
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "The Boys"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
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
	if err := database.SetIncidentFinding(ctx, inc.ID, finding, finding); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded); err != nil {
		t.Fatal(err)
	}
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

func TestEscalationPreview_Success(t *testing.T) {
	t.Parallel()
	ag := &stubEscalationAgent{planResult: map[string]any{"would_delete": []string{"ep01.mkv"}}}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview")
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

	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "No Finding Show"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/media/incidents/" + inc.ID + "/escalation-preview")
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
	ag := &stubEscalationAgent{runResult: map[string]any{"deleted_files": []string{"ep01.mkv"}}}
	srv, database := newEscalationTestServer(t, ag)
	inc := newManualTestNeededIncident(t, database)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation", "", nil)
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

	resp, err := ts.Client().Post(ts.URL+"/media/incidents/"+inc.ID+"/approve-escalation", "", nil)
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
