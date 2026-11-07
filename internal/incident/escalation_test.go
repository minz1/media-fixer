package incident_test

import (
	"context"
	"database/sql"
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
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
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
	ctx context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	if a.runBlocks {
		a.runStart <- struct{}{}
		<-ctx.Done()
	}
	return gatedExit(ctx, gate, nil, nil, ctx.Err())
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

func (a *escalationAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult, _ string) (any, error) {
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
	lastID string
	svc    *incident.Service
	db     *db.DB
	jrnl   *journal.Journal
	dbPath string
}

func newEscalationFixture(t *testing.T, ag incident.AgentRunner) *escalationFixture {
	t.Helper()
	return newEscalationFixtureNotifier(t, ag, &captureNotifier{})
}

func newEscalationFixtureNotifier(
	t *testing.T, ag incident.AgentRunner, notif *captureNotifier,
) *escalationFixture {
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
	svc := incident.NewService(base, database, jrnl, ag, nil, nil, notif, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopService(t, svc) })
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
	_, fp, err := fx.svc.PreviewEscalation(context.Background(), id, fx.svc.CurrentVersion(context.Background(), id))
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

func waitForStatusEvent(t *testing.T, ch <-chan *db.Event, after string) {
	t.Helper()
	deadline := time.After(notifyWaitTimeout)
	for {
		select {
		case e := <-ch:
			if e.Kind == string(journal.KindStatusChanged) {
				return
			}
		case <-deadline:
			t.Fatalf("after %s: no status_changed event", after)
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
	wg.Go(func() {
		errs[0] = fx.svc.ApproveEscalation(
			context.Background(),
			inc.ID,
			fp,
			fx.svc.CurrentVersion(context.Background(), inc.ID),
		)
	})
	<-ag.execStart
	wg.Go(func() {
		errs[1] = fx.svc.ApproveEscalation(
			context.Background(),
			inc.ID,
			fp,
			fx.svc.CurrentVersion(context.Background(), inc.ID),
		)
	})
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

func TestApproveEscalation_PlanClaimedBeforeExecute(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)

	done := make(chan error, 1)
	go func() {
		done <- fx.svc.ApproveEscalation(context.Background(), inc.ID, fp, fx.svc.CurrentVersion(context.Background(), inc.ID))
	}()
	<-ag.execStart
	during := status(t, fx.db, inc.ID)
	again := fx.svc.ApproveEscalation(
		context.Background(),
		inc.ID,
		fp,
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	)
	close(ag.execGate)
	if approveErr := <-done; approveErr != nil {
		t.Fatal(approveErr)
	}
	if during != db.IncidentStatus(lifecycle.ExecutingPlan) || again == nil {
		t.Errorf("during execution: status %s, second approve %v; want executing_plan and a refusal", during, again)
	}
}

func TestApproveEscalation_PreviewDuringRunRefused(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)

	done := make(chan error, 1)
	go func() {
		done <- fx.svc.ApproveEscalation(context.Background(), inc.ID, fp, fx.svc.CurrentVersion(context.Background(), inc.ID))
	}()
	<-ag.execStart
	_, _, previewErr := fx.svc.PreviewEscalation(
		context.Background(),
		inc.ID,
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	)
	close(ag.execGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(previewErr, incident.ErrNotAwaitingDecisionForTest()) {
		t.Errorf("preview during the run: %v, want a refusal", previewErr)
	}
	if _, _, err := fx.db.GetEscalationPlan(context.Background(), inc.ID); !errors.Is(err, db.ErrNoEscalationPlan) {
		t.Errorf("a plan survived the run: err = %v", err)
	}
}

func TestPreviewEscalation_RefusedWhenNotManualTestNeeded(t *testing.T) {
	t.Parallel()
	fx := newEscalationFixture(t, newEscalationAgent())
	inc := fx.incident(t)
	if err := fx.db.UpdateIncidentStatus(context.Background(), inc.ID, db.StatusReopened); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fx.svc.PreviewEscalation(
		context.Background(),
		inc.ID,
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	); !errors.Is(
		err,
		incident.ErrNotAwaitingDecisionForTest(),
	) {
		t.Errorf("err = %v, want errNotAwaitingDecision", err)
	}
	if _, _, err := fx.db.GetEscalationPlan(context.Background(), inc.ID); !errors.Is(err, db.ErrNoEscalationPlan) {
		t.Errorf("a refused preview stored a plan: err = %v", err)
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

	err := fx.svc.ApproveEscalation(
		context.Background(),
		inc.ID,
		fp,
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	)
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

	err := fx.svc.ApproveEscalation(
		context.Background(),
		inc.ID,
		"000000000000",
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	)
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
		{"Resolve", func() error { return svc.Resolve(ctx, inc.ID, incident.Version{}) }},
		{"Rerun", func() error { return svc.Rerun(ctx, inc.ID, incident.Version{}) }},
	} {
		if setErr := database.SetEscalationPlan(ctx, inc.ID, []byte(`{"Title":"x"}`)); !errors.Is(
			setErr, db.ErrPlanNotStored) {
			t.Fatalf("before %s: plan stored on a %s incident: %v", step.name, inc.Status, setErr)
		}
		if stErr := database.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded); stErr != nil {
			t.Fatal(stErr)
		}
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
	waitForStatusEvent(t, ch, "Preview")

	if err := fx.svc.ApproveEscalation(
		ctx,
		inc.ID,
		fp,
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	); err == nil {
		t.Fatal("Approve succeeded, want the execution error")
	}
	waitForStatusEvent(t, ch, "failed Approve")

	fp = fx.preview(t, inc.ID)
	waitForStatusEvent(t, ch, "second Preview")
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
		fx.svc.CurrentVersion(ctx, inc.ID),
	); !errors.Is(
		approveErr,
		incident.ErrPlanExpiredForTest(),
	) {
		t.Fatalf("err = %v, want errPlanExpired", approveErr)
	}
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
	err := fx.svc.ApproveEscalation(ctx, inc.ID, "000000000000", fx.svc.CurrentVersion(context.Background(), inc.ID))
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
	if resolveErr := svc.Resolve(ctx, inc.ID, incident.Version{}); resolveErr != nil {
		t.Fatal(resolveErr)
	}
	msgs, resolved, comments := len(notif.msgs), len(seerr.resolved), len(seerr.comments["7"])

	if resolveErr := svc.Resolve(ctx, inc.ID, incident.Version{}); !errors.Is(resolveErr, incident.ErrAlreadyResolved) {
		t.Fatalf("second Resolve err = %v, want ErrAlreadyResolved", resolveErr)
	}
	if len(notif.msgs) != msgs || len(seerr.resolved) != resolved || len(seerr.comments["7"]) != comments {
		t.Errorf("second Resolve notified again: msgs %d→%d, seerr resolved %d→%d, comments %d→%d",
			msgs, len(notif.msgs), resolved, len(seerr.resolved), comments, len(seerr.comments["7"]))
	}
}
