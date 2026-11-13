package incident_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

var errStoreDown = errors.New("store down")

type flakyStore struct {
	inner          *db.DB
	mu             sync.Mutex
	writes         int
	failFrom       int
	spareInterrupt bool
}

func (s *flakyStore) ApplyOperation(
	ctx context.Context, id int64, decide func(operation.Op) (operation.Op, error),
) (operation.Op, error) {
	return s.inner.ApplyOperation(ctx, id, func(cur operation.Op) (operation.Op, error) {
		next, err := decide(cur)
		if err != nil {
			return next, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.writes++
		interrupt := next.Status == operation.FailedAtStep && next.Cause == operation.CauseStore
		if s.failFrom > 0 && s.writes >= s.failFrom && (!interrupt || !s.spareInterrupt) {
			return operation.Op{}, errStoreDown
		}
		return next, nil
	})
}

func incidentOp(t *testing.T, fx *approvedFixture, id string) operation.Op {
	t.Helper()
	op, err := fx.db.LatestOperation(context.Background(), operation.Subject{Kind: operation.SubjectIncident, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func twoFiles() client.ReplacePlan {
	return seasonPlan(
		client.ReplaceFile{ID: 10, Path: "/mnt/s01e01.mkv"},
		client.ReplaceFile{ID: 11, Path: "/mnt/s01e02.mkv"},
	)
}

func snapOf(t *testing.T, fx *approvedFixture, id string) lifecycle.Snapshot {
	t.Helper()
	s, err := fx.db.GetIncidentSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func states(op operation.Op) []operation.StepState {
	out := make([]operation.StepState, len(op.Results))
	for i, r := range op.Results {
		out[i] = r.State
	}
	return out
}

func TestApprove_StepsDurableAcrossTheRun(t *testing.T) {
	t.Parallel()
	var fx *approvedFixture
	var id string
	var mu sync.Mutex
	seen := map[int][]operation.StepState{}
	arr := &fakeArr{onDelete: func(file int) {
		got := states(incidentOp(t, fx, id))
		mu.Lock()
		defer mu.Unlock()
		seen[file] = got
	}}
	fx = newApprovedFixture(t, arr)
	inc := fx.awaiting(t, replaceParams())
	id = inc.ID

	if err := fx.approve(t, id, twoFiles()); err != nil {
		t.Fatal(err)
	}

	pending, started, done := operation.ResultPending, operation.ResultStarted, operation.ResultDone
	mu.Lock()
	got := seen[11]
	mu.Unlock()
	if len(got) != 3 || got[0] != done || got[1] != started || got[2] != pending {
		t.Errorf("during the second delete the op reads %v, want [done started pending]", got)
	}
	if op := incidentOp(t, fx, id); op.Status != operation.Done {
		t.Errorf("op status = %s, want done", op.Status)
	}
	if s := snapOf(t, fx, id); s.Status != lifecycle.Verifying {
		t.Errorf("incident = %s, want verifying", s.Status)
	}
}

func TestS9_4_PartialDeleteLandsInPlanPartialWithResume(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{failDelete: map[int]bool{11: true}})
	inc := fx.awaiting(t, replaceParams())

	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the delete failure")
	}

	s := snapOf(t, fx, inc.ID)
	if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonPlanPartial {
		t.Fatalf("incident = %s/%s, want manual_test_needed/plan_partial", s.Status, s.Reason)
	}
	if !operation.Resumable(incidentOp(t, fx, inc.ID)) {
		t.Fatal("op is not resumable")
	}
}

func TestS9_6_SearchNotSentLandsInPlanPartialWithResume(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{failSearch: true})
	inc := fx.awaiting(t, replaceParams())

	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the search failure")
	}

	s := snapOf(t, fx, inc.ID)
	if s.Reason != lifecycle.ReasonPlanPartial || !operation.Resumable(incidentOp(t, fx, inc.ID)) {
		t.Fatalf("incident = %s op=%v, want plan_partial with a resumable op", s.Reason, s.Op)
	}
	got := states(incidentOp(t, fx, inc.ID))
	if got[0] != operation.ResultDone || got[1] != operation.ResultDone || got[2] != operation.ResultFailed {
		t.Errorf("steps = %v, want [done done failed]", got)
	}
}

func TestApprove_NothingAppliedOffersPreviewAgain(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{failDelete: map[int]bool{10: true}, deleteState: 400})
	inc := fx.awaiting(t, replaceParams())

	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the delete failure")
	}

	s := snapOf(t, fx, inc.ID)
	if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonPlanFailed {
		t.Fatalf("incident = %s/%s, want manual_test_needed/plan_failed", s.Status, s.Reason)
	}
	if operation.Resumable(incidentOp(t, fx, inc.ID)) {
		t.Error("op is resumable, want nothing applied")
	}
}

func TestApprove_OneLogRowPerExecution(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{failDelete: map[int]bool{11: true}})
	inc := fx.awaiting(t, replaceParams())

	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the delete failure")
	}

	got := fx.actions(t, inc.ID)
	if len(got) != 1 || got[0].Status != db.ActionFailed || got[0].TriggeredBy != "owner" {
		t.Fatalf("rows = %s, want one failed owner row", describeActions(got))
	}
}

func TestApprove_StoreFailureInterruptsOp(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{})
	fx.svc.SetOpStoreForTest(&flakyStore{inner: fx.db, failFrom: 4, spareInterrupt: true})
	inc := fx.awaiting(t, replaceParams())

	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the store failure")
	}

	op := incidentOp(t, fx, inc.ID)
	if op.Status != operation.FailedAtStep || op.Cause != operation.CauseStore {
		t.Errorf("op = %s/%s, want failed_at_step/store", op.Status, op.Cause)
	}
	s := snapOf(t, fx, inc.ID)
	if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonPlanPartial {
		t.Errorf("incident = %s/%s, want manual_test_needed/plan_partial", s.Status, s.Reason)
	}
	if cause, ok := fx.svc.LostCauseForTest(inc.ID); ok {
		t.Errorf("lostCause = %s, want none after a settled run", cause)
	}
}

func TestApprove_StoreFailureRecordsLostCause(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{})
	fx.svc.SetOpStoreForTest(&flakyStore{inner: fx.db, failFrom: 4})
	inc := fx.awaiting(t, replaceParams())

	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the store failure")
	}

	if cause, ok := fx.svc.LostCauseForTest(inc.ID); !ok || cause != operation.CauseStore {
		t.Errorf("lostCause = %q %v, want store", cause, ok)
	}
	if op := incidentOp(t, fx, inc.ID); op.Status != operation.Running {
		t.Errorf("op = %s, want running (nothing could write)", op.Status)
	}
	if s := snapOf(t, fx, inc.ID); s.Status != lifecycle.ExecutingPlan {
		t.Errorf("incident = %s, want executing_plan for the reconciler", s.Status)
	}
	if fx.svc.PlanLiveForTest(inc.ID) {
		t.Error("planLive still set after the section")
	}
}

func searchSpec(subject operation.Subject, source []byte) operation.Spec {
	return operation.Spec{
		Subject: subject, Kind: operation.KindReplace, Title: "The Boys", Source: source,
		Steps: []operation.Step{{
			Kind: operation.StepSearch, App: "sonarr", MediaID: 1, Command: "SeasonSearch", Season: 1,
		}},
	}
}

type agentArr struct {
	opexec.Arr

	ag approvedRunner
}

func (a agentArr) Search(ctx context.Context, _ string, _, _ int, _ []int) error {
	if _, err := a.ag.runApproved(ctx, nil, nil); err != nil {
		return &client.HTTPStatusError{Status: http.StatusBadRequest, Body: err.Error()}
	}
	return nil
}

func execPorts(ag approvedRunner) opexec.Ports {
	return opexec.Ports{Arr: func(string) opexec.Arr { return agentArr{ag: ag} }}
}

type approvedRunner interface {
	incident.AgentRunner
	runApproved(ctx context.Context, result *agent.DiagnosticResult, planJSON []byte) (any, error)
}

func TestApprove_WithoutSpecLogsAnError(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	fx := newEscalationFixtureLogger(
		t, newEscalationAgent(), &captureNotifier{}, slog.New(slog.NewTextHandler(logs, nil)),
	)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	err := fx.svc.ApplyBareApproveForTest(context.Background(), inc.ID, fp)
	if !errors.Is(err, lifecycle.ErrNoSpec) {
		t.Fatalf("err = %v", err)
	}
	var got []string
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=ERROR") {
			got = append(got, line)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "no_spec") || !strings.Contains(got[0], inc.ID) {
		t.Fatalf("error records = %q", got)
	}
	if incident.IsRefusal(err) {
		t.Error("ErrNoSpec counted as an owner refusal")
	}
}
