package incident_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

type planAgent struct {
	*escalationAgent

	fx   *escalationFixture
	seen atomic.Value
}

func (a *planAgent) runApproved(ctx context.Context, r *agent.DiagnosticResult, plan []byte) (any, error) {
	if a.fx != nil {
		a.seen.Store(a.fx.svc.PlanLiveForTest(a.fx.lastID))
	}
	_, err := a.escalationAgent.runApproved(ctx, r, plan)
	return nil, err
}

func (fx *escalationFixture) withAction(t *testing.T, action string) *db.Incident {
	t.Helper()
	inc := fx.incident(t)
	finding := agent.DiagnosticResult{
		RootCause: "bad", PrimaryAction: "manual_investigation", EscalateAction: action,
		EscalateParams: map[string]any{"media_type": "tv", "title": "The Boys"},
	}
	lifecycletest.Finding(t, fx.db, inc.ID, finding)
	return inc
}

func (fx *escalationFixture) snap(t *testing.T, id string) lifecycle.Snapshot {
	t.Helper()
	s, err := fx.db.GetIncidentSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (fx *escalationFixture) approveNow(id, fp string) error {
	return fx.svc.ApproveEscalation(context.Background(), id, fp, fx.svc.CurrentVersion(context.Background(), id))
}

func failedPlan(t *testing.T, action string) lifecycle.Snapshot {
	t.Helper()
	ag := &planAgent{escalationAgent: newEscalationAgent()}
	ag.execErr = errors.New("step 2 failed")
	fx := newEscalationFixture(t, ag)
	inc := fx.withAction(t, action)
	fp := fx.preview(t, inc.ID)
	if err := fx.approveNow(inc.ID, fp); err == nil {
		t.Fatal("a failing plan reported success")
	}
	return fx.snap(t, inc.ID)
}

func TestI1_19_ServiceRerunRefusedDuringPlan(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	done := make(chan error, 1)
	go func() { done <- fx.approveNow(inc.ID, fp) }()
	<-ag.execStart
	err := fx.svc.Rerun(context.Background(), inc.ID, fx.svc.CurrentVersion(context.Background(), inc.ID))
	close(ag.execGate)
	<-done
	if !errors.Is(err, lifecycle.ErrPlanRunning) {
		t.Fatalf("rerun during the plan: %v", err)
	}
}

func TestI3_8_ResolveRefusedDuringPlan(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	done := make(chan error, 1)
	go func() { done <- fx.approveNow(inc.ID, fp) }()
	<-ag.execStart
	err := fx.svc.Resolve(context.Background(), inc.ID, incident.Version{})
	close(ag.execGate)
	<-done
	if !errors.Is(err, lifecycle.ErrPlanRunning) {
		t.Fatalf("resolve during the plan: %v", err)
	}
}

func TestI5_11_ApproveCannotSupersedeRerun(t *testing.T) {
	t.Parallel()
	fx := newEscalationFixture(t, newEscalationAgent())
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	old := fx.svc.CurrentVersion(context.Background(), inc.ID)
	if err := fx.svc.Rerun(
		context.Background(),
		inc.ID,
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.ApproveEscalation(context.Background(), inc.ID, fp, old); err == nil {
		t.Fatal("an approve after a rerun ran the old plan")
	}
	if s := fx.snap(t, inc.ID); s.Status == lifecycle.ExecutingPlan {
		t.Fatal("the approve superseded the rerun")
	}
}

func TestI6_10_ButtonsDuringPlanExecution(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	done := make(chan error, 1)
	go func() { done <- fx.approveNow(inc.ID, fp) }()
	<-ag.execStart
	s := fx.snap(t, inc.ID)
	close(ag.execGate)
	<-done
	var kinds []lifecycle.ActionKind
	for _, a := range lifecycle.Actions(s, time.Now()) {
		kinds = append(kinds, a.Kind)
	}
	if len(kinds) != 0 {
		t.Fatalf("actions during the plan %v, want none", kinds)
	}
}

func TestApprove_PlanFailedNothingAppliedLandsInPlanFailed(t *testing.T) {
	t.Parallel()
	s := failedPlan(t, agent.EscalateRemoveAndSearch)
	if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonPlanFailed || s.Plan != nil {
		t.Fatalf("after a failed plan with nothing applied: %s/%s plan=%v", s.Status, s.Reason, s.Plan != nil)
	}
	if !lifecycle.Offers(s, time.Now(), lifecycle.ActPreview) {
		t.Fatal("plan_failed does not offer Preview again")
	}
}

func TestApprove_ClaimAndStateOneWrite(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	before := fx.snap(t, inc.ID)
	done := make(chan error, 1)
	go func() { done <- fx.approveNow(inc.ID, fp) }()
	<-ag.execStart
	during := fx.snap(t, inc.ID)
	close(ag.execGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if during.Status != lifecycle.ExecutingPlan || during.Plan == nil || during.Generation != before.Generation+1 ||
		during.Version != before.Version+1 {
		t.Fatalf("claim: %+v", during)
	}
}

func TestApprove_PlanLiveSetBeforeApply(t *testing.T) {
	t.Parallel()
	ag := &planAgent{escalationAgent: newEscalationAgent()}
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	ag.fx = fx
	inc := fx.incident(t)
	fx.lastID = inc.ID
	fp := fx.preview(t, inc.ID)
	stop := make(chan struct{})
	var bad atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if s, err := fx.db.GetIncidentSnapshot(context.Background(), inc.ID); err != nil ||
				s.Status != lifecycle.ExecutingPlan || fx.svc.PlanLiveForTest(inc.ID) {
				continue
			}
			if s, err := fx.db.GetIncidentSnapshot(context.Background(), inc.ID); err == nil &&
				s.Status == lifecycle.ExecutingPlan {
				bad.Store(true)
			}
		}
	})
	done := make(chan error, 1)
	go func() { done <- fx.approveNow(inc.ID, fp) }()
	<-ag.execStart
	close(ag.execGate)
	<-done
	close(stop)
	wg.Wait()
	if bad.Load() || ag.seen.Load() != true {
		t.Fatal("executing_plan was visible without planLive")
	}
	if fx.svc.PlanLiveForTest(inc.ID) {
		t.Fatal("planLive stayed set after the plan finished")
	}
}
