package incident_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
)

const (
	escalationSettle = 50 * time.Millisecond
	staleRefusalWait = 2 * time.Second
	expiredPlanAge   = 2 * time.Hour
)

type escalationAgent struct {
	runBlocks bool
	runStart  chan struct{}
	execGate  chan struct{}
	execStart chan struct{}
	execErr   error
	execCalls atomic.Int32
}

func newEscalationAgent() *escalationAgent {
	return &escalationAgent{runStart: make(chan struct{}, 1), execStart: make(chan struct{}, 2)}
}

func (a *escalationAgent) Run(
	ctx context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	if a.runBlocks {
		a.runStart <- struct{}{}
		<-ctx.Done()
	}
	return nil, nil, ctx.Err()
}

func (a *escalationAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	return false
}

func (a *escalationAgent) ScanRunning(_ context.Context) bool { return false }

func (a *escalationAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *escalationAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{"Title": "Previewed"}, nil
}

func (a *escalationAgent) ExecuteApprovedPlan(_ context.Context, _ *agent.DiagnosticResult, _ []byte) (any, error) {
	a.execCalls.Add(1)
	a.execStart <- struct{}{}
	if a.execGate != nil {
		<-a.execGate
	}
	return map[string]any{}, a.execErr
}

func (a *escalationAgent) CheckPendingOutcome(
	_ context.Context, _ *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return &agent.PendingOutcomeObservation{}, nil
}

type escalationFixture struct {
	svc    *incident.Service
	db     *db.DB
	jrnl   *journal.Journal
	dbPath string
}

func newEscalationFixture(t *testing.T, ag incident.AgentRunner) *escalationFixture {
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
	base, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	jrnl := journal.New(database)
	svc := incident.NewService(base, database, jrnl, ag, nil, nil, &captureNotifier{}, slog.New(slog.DiscardHandler))
	return &escalationFixture{svc: svc, db: database, jrnl: jrnl, dbPath: f.Name()}
}

func (fx *escalationFixture) incident(t *testing.T) *db.Incident {
	t.Helper()
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "The Boys"}
	if err := fx.db.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	finding := agent.DiagnosticResult{
		RootCause:      "bad release",
		PrimaryAction:  "manual_investigation",
		EscalateAction: agent.EscalateRemoveAndSearch,
		EscalateParams: map[string]any{"media_type": "tv", "title": "The Boys"},
	}
	if err := fx.db.SetIncidentFinding(ctx, inc.ID, finding, finding); err != nil {
		t.Fatal(err)
	}
	if err := fx.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded); err != nil {
		t.Fatal(err)
	}
	return inc
}

func (fx *escalationFixture) preview(t *testing.T, id string) string {
	t.Helper()
	_, fp, err := fx.svc.PreviewEscalation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := fx.db.GetEscalationPlan(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := card.Fingerprint(stored); fp != want {
		t.Fatalf("preview fp = %q, want fingerprint of the stored bytes %q", fp, want)
	}
	return fp
}

func waitForEmptyStatusChanged(t *testing.T, ch <-chan *db.Event, after string) {
	t.Helper()
	deadline := time.After(notifyWaitTimeout)
	for {
		select {
		case e := <-ch:
			if e.Kind != string(journal.KindStatusChanged) {
				continue
			}
			var p struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Status == "" {
				return
			}
		case <-deadline:
			t.Fatalf("after %s: no status_changed event with an empty status", after)
		}
	}
}

func TestApproveEscalation_ConcurrentRunsOnce(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Go(func() { errs[0] = fx.svc.ApproveEscalation(context.Background(), inc.ID, fp) })
	<-ag.execStart
	wg.Go(func() { errs[1] = fx.svc.ApproveEscalation(context.Background(), inc.ID, fp) })
	time.Sleep(escalationSettle)
	close(ag.execGate)
	wg.Wait()

	if n := ag.execCalls.Load(); n != 1 {
		t.Fatalf("ExecuteApprovedPlan ran %d times, want 1", n)
	}
	if errs[0] != nil || errs[1] == nil {
		t.Errorf("errs = %v, want the first to succeed and the second to be refused", errs)
	}
}

func TestApproveEscalation_RefusedWhenNotManualTestNeeded(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	if err := fx.db.UpdateIncidentStatus(context.Background(), inc.ID, db.StatusAgentFixed); err != nil {
		t.Fatal(err)
	}

	err := fx.svc.ApproveEscalation(context.Background(), inc.ID, fp)
	if !errors.Is(err, incident.ErrNotAwaitingDecisionForTest()) {
		t.Errorf("err = %v, want errNotAwaitingDecision", err)
	}
	if n := ag.execCalls.Load(); n != 0 {
		t.Errorf("ExecuteApprovedPlan ran %d times, want 0", n)
	}
}

func TestApproveEscalation_RefusedOnFingerprintMismatch(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fx.preview(t, inc.ID)

	err := fx.svc.ApproveEscalation(context.Background(), inc.ID, "000000000000")
	if !errors.Is(err, incident.ErrPlanChangedForTest()) {
		t.Errorf("err = %v, want errPlanChanged", err)
	}
	if n := ag.execCalls.Load(); n != 0 {
		t.Errorf("ExecuteApprovedPlan ran %d times, want 0", n)
	}
}

func TestRerunAndResolve_ClearStoredPlan(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()
	inc, err := svc.Handle(
		ctx,
		&incident.Report{Source: "discord", ReportedBy: "a", What: "cant_play", Title: "Deadwood"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"Resolve", func() error { return svc.Resolve(ctx, inc.ID) }},
		{"Rerun", func() error { return svc.Rerun(ctx, inc.ID) }},
	} {
		if setErr := database.SetEscalationPlan(ctx, inc.ID, []byte(`{"Title":"x"}`)); setErr != nil {
			t.Fatal(setErr)
		}
		if runErr := step.run(); runErr != nil {
			t.Fatal(runErr)
		}
		if _, _, getErr := database.GetEscalationPlan(ctx, inc.ID); !errors.Is(getErr, db.ErrNoEscalationPlan) {
			t.Errorf("after %s: GetEscalationPlan err = %v, want ErrNoEscalationPlan", step.name, getErr)
		}
	}
}

func TestPreviewAndApprove_EmitStatusChanged(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execErr = errors.New("boom")
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	ctx := context.Background()
	ch, unsubscribe := fx.jrnl.Subscribe(inc.ID)
	defer unsubscribe()

	fp := fx.preview(t, inc.ID)
	waitForEmptyStatusChanged(t, ch, "Preview")

	if err := fx.svc.ApproveEscalation(ctx, inc.ID, fp); err == nil {
		t.Fatal("Approve succeeded, want the execution error")
	}
	waitForEmptyStatusChanged(t, ch, "failed Approve")

	fp = fx.preview(t, inc.ID)
	waitForEmptyStatusChanged(t, ch, "second Preview")
	raw, err := sql.Open("sqlite", fx.dbPath+"?_time_format=sqlite&_timezone=UTC")
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := raw.ExecContext(ctx, `UPDATE incidents SET escalation_plan_at = ? WHERE id = ?`,
		time.Now().Add(-expiredPlanAge), inc.ID); execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if approveErr := fx.svc.ApproveEscalation(
		ctx,
		inc.ID,
		fp,
	); !errors.Is(
		approveErr,
		incident.ErrPlanExpiredForTest(),
	) {
		t.Fatalf("err = %v, want errPlanExpired", approveErr)
	}
	waitForEmptyStatusChanged(t, ch, "expired Approve")
}

func TestApproveEscalation_StaleRefusedWithoutWaiting(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.runBlocks = true
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fx.preview(t, inc.ID)
	if _, err := fx.svc.Handle(context.Background(), &incident.Report{
		Source: "discord", ReportedBy: "a", What: "cant_play", Title: "Holder",
	}); err != nil {
		t.Fatal(err)
	}
	<-ag.runStart

	ctx, cancel := context.WithTimeout(context.Background(), staleRefusalWait)
	defer cancel()
	err := fx.svc.ApproveEscalation(ctx, inc.ID, "000000000000")
	if !errors.Is(err, incident.ErrPlanChangedForTest()) {
		t.Errorf("err = %v, want errPlanChanged before waiting for the global slot", err)
	}
}

func TestResolve_AlreadyResolvedRefuses(t *testing.T) {
	t.Parallel()
	svc, _, notif := newTestService(t)
	seerr := &fakeSeerr{}
	svc.SetSeerr(seerr)
	ctx := context.Background()
	inc, err := svc.Handle(ctx, seerrReport("7"))
	if err != nil {
		t.Fatal(err)
	}
	if resolveErr := svc.Resolve(ctx, inc.ID); resolveErr != nil {
		t.Fatal(resolveErr)
	}
	msgs, resolved, comments := len(notif.msgs), len(seerr.resolved), len(seerr.comments["7"])

	if resolveErr := svc.Resolve(ctx, inc.ID); !errors.Is(resolveErr, incident.ErrAlreadyResolved) {
		t.Fatalf("second Resolve err = %v, want ErrAlreadyResolved", resolveErr)
	}
	if len(notif.msgs) != msgs || len(seerr.resolved) != resolved || len(seerr.comments["7"]) != comments {
		t.Errorf("second Resolve notified again: msgs %d→%d, seerr resolved %d→%d, comments %d→%d",
			msgs, len(notif.msgs), resolved, len(seerr.resolved), comments, len(seerr.comments["7"]))
	}
}
