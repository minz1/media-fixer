package incident_test

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

type rebooted struct {
	svc   *incident.Service
	arr   *resumeArr
	decy  *resumeDecy
	notif *syncNotifier
}

func reboot(t *testing.T, database *db.DB) *rebooted {
	t.Helper()
	r := &rebooted{arr: &resumeArr{}, decy: &resumeDecy{}, notif: newSyncNotifier()}
	jrnl := journal.New(database)
	log := slog.New(slog.DiscardHandler)
	ag := agent.New(nil, "stub", &agent.Dispatcher{}, database, jrnl, log)
	r.svc = incident.NewService(context.Background(), database, jrnl, ag, nil, nil, r.notif, log)
	r.svc.SetOpPorts(opexec.Ports{Arr: func(string) opexec.Arr { return r.arr }, Decypharr: r.decy})
	t.Cleanup(func() { stopService(t, r.svc) })
	return r
}

func (r *rebooted) calls() int {
	r.arr.mu.Lock()
	defer r.arr.mu.Unlock()
	r.decy.mu.Lock()
	defer r.decy.mu.Unlock()
	return len(r.arr.calls) + len(r.decy.calls)
}

func crashMidRun(t *testing.T, failFrom int) (*approvedFixture, string) {
	t.Helper()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	fx.svc.SetOpStoreForTest(&flakyStore{inner: fx.db, failFrom: failFrom})
	inc := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())
	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the store to stop mid-run")
	}
	shutdownNow(t, fx.svc)
	return fx, inc.ID
}

func startupInterrupt(t *testing.T, database *db.DB) []operation.Op {
	t.Helper()
	ops, err := database.InterruptRunningOperations(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func snapWithOp(t *testing.T, database *db.DB, id string) lifecycle.Snapshot {
	t.Helper()
	s, err := database.GetIncidentSnapshotWithOp(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOperation_NeverResumedAtStartup(t *testing.T) {
	t.Parallel()
	fx, id := crashMidRun(t, 4)
	if op := incidentOp(t, fx, id); op.Status != operation.Running {
		t.Fatalf("op = %s before boot, want running", op.Status)
	}

	if ops := startupInterrupt(t, fx.db); len(ops) != 1 {
		t.Fatalf("startup interrupted %d ops, want 1", len(ops))
	}
	boot := reboot(t, fx.db)
	boot.svc.StartRecovery()
	boot.svc.ReconcileForTest(context.Background())
	boot.svc.ReconcileForTest(context.Background())

	if n := boot.calls(); n != 0 {
		t.Errorf("fakes saw %d calls after startup and two ticks, want 0", n)
	}
	op := incidentOp(t, fx, id)
	if op.Status != operation.FailedAtStep || op.Cause != operation.CauseRestart {
		t.Errorf("op = %s/%s, want failed_at_step/restart", op.Status, op.Cause)
	}
	s := snapWithOp(t, fx.db, id)
	if s.Reason != lifecycle.ReasonPlanInterrupted || !lifecycle.Offers(s, time.Now(), lifecycle.ActResume) {
		t.Errorf("incident = %s/%s resume=%v, want plan_interrupted offering Resume", s.Status, s.Reason,
			lifecycle.Offers(s, time.Now(), lifecycle.ActResume))
	}
}

func TestRestart_KillDuringEachStepPoint(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name     string
		failFrom int
		reason   lifecycle.Reason
		resume   bool
	}{
		{"before step 0 started", 1, lifecycle.ReasonPlanFailed, false},
		{"before step 1 started", 3, lifecycle.ReasonPlanInterrupted, true},
		{"during step 1 call", 4, lifecycle.ReasonPlanInterrupted, true},
		{"before step 2 started", 5, lifecycle.ReasonPlanInterrupted, true},
		{"during step 2 call", 6, lifecycle.ReasonPlanInterrupted, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fx, id := crashMidRun(t, row.failFrom)

			startupInterrupt(t, fx.db)
			boot := reboot(t, fx.db)
			boot.svc.ReconcileForTest(context.Background())

			s := snapWithOp(t, fx.db, id)
			if s.Status != lifecycle.ManualTestNeeded || s.Reason != row.reason {
				t.Errorf("incident = %s/%s, want manual_test_needed/%s", s.Status, s.Reason, row.reason)
			}
			if got := lifecycle.Offers(s, time.Now(), lifecycle.ActResume); got != row.resume {
				t.Errorf("Resume offered = %v, want %v", got, row.resume)
			}
			op := incidentOp(t, fx, id)
			if op.Status != operation.FailedAtStep || op.Cause != operation.CauseRestart {
				t.Errorf("op = %s/%s, want failed_at_step/restart", op.Status, op.Cause)
			}
			if n := boot.calls(); n != 0 {
				t.Errorf("the reboot made %d calls, want 0", n)
			}
		})
	}
}

func TestRestart_KillAfterLastResultBeforePlanExecuted(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	inc := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())
	if err := fx.approve(t, inc.ID, twoFiles()); err != nil {
		t.Fatal(err)
	}
	shutdownNow(t, fx.svc)
	done := incidentOp(t, fx, inc.ID)
	lifecycletest.Force(t, fx.db, inc.ID, func(n *lifecycle.Snapshot) {
		n.Status, n.Verify, n.NextCheck = lifecycle.ExecutingPlan, nil, time.Time{}
		n.Op = &done
		n.Plan = &lifecycle.Plan{JSON: done.Source, At: time.Now()}
	})

	startupInterrupt(t, fx.db)
	boot := reboot(t, fx.db)
	boot.svc.ReconcileForTest(context.Background())

	if s := snapWithOp(t, fx.db, inc.ID); s.Status != lifecycle.Verifying {
		t.Errorf("incident = %s, want verifying as PlanExecuted would have left it", s.Status)
	}
	if n := boot.calls(); n != 0 {
		t.Errorf("the reboot made %d calls, want 0", n)
	}
}

func TestRestart_ResolvedMidOpThenCrashKeepsNoResume(t *testing.T) {
	t.Parallel()
	fx, id := crashMidRun(t, 4)
	lifecycletest.Force(t, fx.db, id, func(n *lifecycle.Snapshot) {
		n.Status, n.Reason, n.Op, n.Plan, n.Verify, n.NextCheck = lifecycle.Resolved, lifecycle.ReasonNone, nil, nil, nil, time.Time{}
	})

	startupInterrupt(t, fx.db)
	boot := reboot(t, fx.db)
	boot.svc.ReconcileForTest(context.Background())

	s := snapWithOp(t, fx.db, id)
	if s.Status != lifecycle.Resolved || lifecycle.Offers(s, time.Now(), lifecycle.ActResume) {
		t.Errorf("incident = %s resume=%v, want resolved with no Resume", s.Status,
			lifecycle.Offers(s, time.Now(), lifecycle.ActResume))
	}
	if op := incidentOp(t, fx, id); op.Status != operation.FailedAtStep {
		t.Errorf("op = %s, want failed_at_step", op.Status)
	}
}

func TestReconciler_PlanLostUsesStoreCause(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	fx.svc.SetOpStoreForTest(&flakyStore{inner: fx.db, failFrom: 4})
	inc := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())
	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the store failure")
	}
	if cause, ok := fx.svc.LostCauseForTest(inc.ID); !ok || cause != operation.CauseStore {
		t.Fatalf("lostCause = %q %v, want store", cause, ok)
	}
	fx.svc.SetOpStoreForTest(fx.db)

	fx.svc.ReconcileForTest(context.Background())

	op := incidentOp(t, fx, inc.ID)
	if op.Status != operation.FailedAtStep || op.Cause != operation.CauseStore {
		t.Errorf("op = %s/%s, want failed_at_step/store", op.Status, op.Cause)
	}
	if s := snapOf(t, fx, inc.ID); s.Reason != lifecycle.ReasonPlanInterrupted {
		t.Errorf("incident reason = %s, want plan_interrupted", s.Reason)
	}
	if cause, ok := fx.svc.LostCauseForTest(inc.ID); ok {
		t.Errorf("lostCause = %q after the PlanLost it fed was applied, want cleared", cause)
	}
}

func TestReconciler_PlanLostDefaultsToRestartCause(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	fx.svc.SetOpStoreForTest(&flakyStore{inner: fx.db, failFrom: 4})
	inc := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())
	if err := fx.approve(t, inc.ID, twoFiles()); err == nil {
		t.Fatal("approve succeeded, want the store failure")
	}
	fx.svc.ClearLostForTest(inc.ID)
	fx.svc.SetOpStoreForTest(fx.db)

	fx.svc.ReconcileForTest(context.Background())

	if op := incidentOp(t, fx, inc.ID); op.Cause != operation.CauseRestart {
		t.Errorf("op cause = %s, want restart", op.Cause)
	}
}

func TestReconciler_PlanLiveStartClearsStaleLostCause(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(
		t, env.db, lcReport("Stale cause"), lifecycletest.Spec{State: lifecycle.ExecutingPlan},
	)
	env.svc.MarkLostForTest(s.ID, operation.CauseStore)

	done := env.svc.PlanStartedForTest(s.ID)
	defer done()

	if cause, ok := env.svc.LostCauseForTest(s.ID); ok {
		t.Errorf("lostCause = %q after planLive started again, want cleared", cause)
	}
}

func TestReconciler_PlanLostRechecksPlanLiveBeforeApply(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(
		t, env.db, lcReport("Fresh section"), lifecycletest.Spec{State: lifecycle.ExecutingPlan},
	)
	var done func()
	env.svc.SetBeforePlanLostForTest(func(id string) {
		if id == s.ID && done == nil {
			done = env.svc.PlanStartedForTest(id)
		}
	})

	env.svc.ReconcileForTest(t.Context())

	if done == nil {
		t.Fatal("the hook never ran: the reconciler did not decide PlanLost")
	}
	defer done()
	if got := env.snap(t, s.ID); got.Status != lifecycle.ExecutingPlan {
		t.Errorf("incident = %s, want executing_plan untouched by PlanLost", got.Status)
	}
	if cards := env.notif.ownerCards(); len(cards) != 0 {
		t.Errorf("%d owner DMs, want none", len(cards))
	}
}

func randomOp(r *rand.Rand) *operation.Op {
	steps := []operation.Step{
		{Kind: operation.StepDeleteFile, App: "sonarr", FileID: 3, Path: "/tv/e1.mkv", Size: 10, MediaID: 5},
		{Kind: operation.StepSearch, App: "sonarr", Command: "SeasonSearch", MediaID: 5, Season: 1},
	}
	states := operation.StepStates()
	results := make([]operation.Result, len(steps))
	for i := range results {
		results[i].State = states[r.IntN(len(states))]
	}
	statuses, causes := operation.Statuses(), operation.Causes()
	return &operation.Op{
		ID: 1 + r.Int64N(5), Subject: operation.Subject{Kind: operation.SubjectIncident, ID: "x"},
		Kind: operation.KindReplace, Title: "t", Source: []byte(`{}`), Steps: steps, Results: results,
		Index: r.IntN(len(steps)), Status: statuses[r.IntN(len(statuses))], Cause: causes[r.IntN(len(causes))],
		Version: 1 + r.Int64N(5),
	}
}

func randomRow(r *rand.Rand, i int) lifecycle.Observed {
	states, reasons := lifecycle.States(), lifecycle.Reasons()
	s := lifecycle.Snapshot{
		ID: string(rune('a' + i)), Status: states[r.IntN(len(states))], Reason: reasons[r.IntN(len(reasons))],
		Generation: 1 + r.Int64N(3), Version: 1 + r.Int64N(9), Parked: r.IntN(4) == 0,
		In: lifecycle.Inputs{Limits: lifecycletest.Limits()},
	}
	if r.IntN(2) == 0 {
		s.Plan = &lifecycle.Plan{JSON: []byte(`{}`), At: time.Now()}
	}
	if r.IntN(3) != 0 {
		s.Op = randomOp(r)
	}
	return lifecycle.Observed{Snap: s, LastActivity: time.Now().Add(-time.Duration(r.IntN(120)) * time.Minute)}
}

func randomLiveness(r *rand.Rand, rows []lifecycle.Observed) lifecycle.Liveness {
	live := lifecycle.Liveness{
		Runs: map[string]int64{}, Reserved: map[string]bool{}, Plans: map[string]bool{}, Armed: map[string]int64{},
		Floors: map[string]time.Duration{},
	}
	for _, row := range rows {
		id := row.Snap.ID
		if r.IntN(3) == 0 {
			live.Runs[id] = row.Snap.Generation
		}
		live.Reserved[id] = r.IntN(4) == 0
		live.Plans[id] = r.IntN(3) == 0
		if r.IntN(3) == 0 {
			live.Armed[id] = row.Snap.Generation
		}
	}
	return live
}

func TestReconciler_NeverResumesAnOperation(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(11, 29))
	now := time.Now()
	for round := range 400 {
		rows := make([]lifecycle.Observed, 1+r.IntN(8))
		for i := range rows {
			rows[i] = randomRow(r, i)
		}
		byID := map[string]lifecycle.Snapshot{}
		for _, row := range rows {
			byID[row.Snap.ID] = row.Snap
		}
		intents := lifecycle.Reconcile(rows, randomLiveness(r, rows), lifecycle.Policy{
			Now: now, Paused: r.IntN(2) == 0, StaleAfter: 30 * time.Minute, Unit: time.Minute,
		})
		for _, in := range intents {
			if in.Event == nil {
				continue
			}
			k := in.Event.Kind
			if k == lifecycle.OwnerResumeOp || k == lifecycle.OwnerApprove {
				t.Fatalf("round %d: the reconciler decided %s", round, k)
			}
			assertNoResume(t, round, byID[in.ID], k, *in.Event, now)
		}
	}
}

func assertNoResume(
	t *testing.T, round int, cur lifecycle.Snapshot, k lifecycle.EventKind, ev lifecycle.Event, now time.Time,
) {
	t.Helper()
	d, err := lifecycle.Next(cur, ev, now)
	if err != nil {
		return
	}
	for _, eff := range d.Effects {
		if eff.Kind == lifecycle.ExecutePlan {
			t.Fatalf("round %d: %s from %s/%s produced ExecutePlan", round, k, cur.Status, cur.Reason)
		}
	}
	if cur.Op != nil && cur.Op.Status == operation.FailedAtStep && d.OpWrite != nil {
		t.Fatalf("round %d: %s rewrote a failed_at_step op (%s)", round, k, d.OpWrite.Status)
	}
	if d.OpWrite != nil && d.OpWrite.Status == operation.Running {
		t.Fatalf("round %d: %s wrote a running op", round, k)
	}
}
