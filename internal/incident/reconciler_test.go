package incident_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestI1_10_PausedReportLaunchesOnResume(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, func(tm *incident.Timers) { tm.CheckEveryMinutes = 60 })
	env.svc.SetUnitForTest(time.Second)
	env.svc.Start()
	if err := env.svc.SetAutonomousPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	id := env.report(t, "Paused then resumed")
	if s := env.snap(t, id); !s.Parked {
		t.Fatal("paused report did not park")
	}
	if err := env.svc.SetAutonomousPaused(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	waitGateStart(t, ag)
	if s := env.snap(t, id); s.Parked || s.Status != lifecycle.Investigating {
		t.Fatalf("after resume: %s parked=%v", s.Status, s.Parked)
	}
}

func TestA5_6_ResumeLaunchesParked(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Parked rerun"), lifecycletest.Spec{State: lifecycle.AgentFixed})
	if err := env.svc.SetAutonomousPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Rerun(t.Context(), s.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.SetAutonomousPaused(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 1 {
		t.Fatalf("resume reconcile %+v, want one run", c)
	}
	waitGateStart(t, ag)
}

func TestI8_3_OpenAlwaysHasDriver(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	s := env.seed(t, "Driverless")
	if env.svc.ReservedOrRunningForTest(s.ID) {
		t.Fatal("a seeded row already has a run")
	}
	env.svc.ReconcileForTest(t.Context())
	waitGateStart(t, ag)
}

func TestReconciler_SkipsReservedIncident(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	s := env.seed(t, "Reserved")
	release := env.svc.ReserveForTest(s.ID)
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 0 {
		t.Fatalf("a reserved incident was relaunched: %+v", c)
	}
	release()
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 1 {
		t.Fatalf("after release: %+v", c)
	}
}

func TestReconciler_PlanLostMatchesById(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Plan live"), lifecycletest.Spec{State: lifecycle.ExecutingPlan})
	done := env.svc.PlanStartedForTest(s.ID)
	if c := env.svc.ReconcileForTest(t.Context()); c.PlansLost != 0 {
		t.Fatalf("a live plan was declared lost: %+v", c)
	}
	done()
	if c := env.svc.ReconcileForTest(t.Context()); c.PlansLost != 1 {
		t.Fatalf("a dead plan was not declared lost: %+v", c)
	}
	got := env.snap(t, s.ID)
	if got.Status != lifecycle.ManualTestNeeded || got.Reason != lifecycle.ReasonPlanInterrupted || got.Plan != nil {
		t.Fatalf("after plan lost: %s/%s plan=%v", got.Status, got.Reason, got.Plan != nil)
	}
}

func TestReconciler_PlanLostAfterRestart(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(
		t,
		env.db,
		lcReport("Plan crashed"),
		lifecycletest.Spec{State: lifecycle.ExecutingPlan},
	)
	env.svc.StartRecovery()
	eventually(t, "the interrupted card", func() bool {
		cards := env.notif.ownerCards()
		return len(cards) == 1 && strings.Contains(cards[0].Plain(), "interrupted by a restart")
	})
	got := env.snap(t, s.ID)
	if lifecycle.Offers(got, time.Now(), lifecycle.ActPreview) ||
		!lifecycle.Offers(got, time.Now(), lifecycle.ActRerun) {
		t.Fatal("plan_interrupted offers the wrong actions")
	}
}

func TestReconciler_StartupStaleRunNoCard(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	s := env.claimed(t, "Crashed mid run")
	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.ActionCount = 2 })
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 1 {
		t.Fatalf("startup reconcile %+v", c)
	}
	waitGateStart(t, ag)
	if got := env.snap(t, s.ID); got.ActionCount != 2 || len(env.notif.ownerCards()) != 0 {
		t.Fatalf("restart rerun: count %d, %d owner cards", got.ActionCount, len(env.notif.ownerCards()))
	}
}

func TestReconciler_LiveStaleRunSendsCard(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, func(tm *incident.Timers) { tm.StaleRunMinutes = 40 })
	env.svc.SetUnitForTest(time.Minute)
	id := env.report(t, "Hung")
	waitGateStart(t, ag)
	env.exec(t, `UPDATE incident_events SET at = ? WHERE incident_id = ?`, time.Now().Add(-41*time.Minute), id)
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 1 {
		t.Fatalf("stale reconcile %+v", c)
	}
	want := fmt.Sprintf("⏱️ Incident **Hung** (#%s) stopped making progress for 40 min and is being re-run.", id[:8])
	eventually(t, "the stale card", func() bool {
		cards := env.notif.ownerCards()
		return len(cards) == 1 && cards[0].Plain() == want && cards[0].Subject == card.IncidentSubject(id)
	})
}

func TestReconciler_KeepsRunsRetryFloor(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, func(tm *incident.Timers) {
		tm.RetryFirstMinutes, tm.RetrySecondMinutes, tm.StaleRunMinutes = 30, 30, 70
	})
	env.svc.SetUnitForTest(time.Minute)
	id := env.report(t, "Floor")
	waitGateStart(t, ag)
	saveTimers(t, env.db, func(tm *incident.Timers) { tm.StaleRunMinutes = 13 })
	env.exec(t, `UPDATE incident_events SET at = ? WHERE incident_id = ?`, time.Now().Add(-20*time.Minute), id)
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 0 || len(env.notif.ownerCards()) != 0 {
		t.Fatalf("a run inside its retry floor was judged stale at the lowered value: %+v", c)
	}
}

func TestReconciler_TickClearsExpiredPlan(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Old plan"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Finding: lifecycletest.Escalatable(), Plan: []byte(`{"files":[]}`),
	})
	env.exec(t, `UPDATE incidents SET escalation_plan_at = ? WHERE id = ?`, time.Now().Add(-2*time.Hour), s.ID)
	env.svc.ReconcileForTest(t.Context())
	if got := env.snap(t, s.ID); got.Plan != nil {
		t.Fatal("an expired plan survived the tick")
	}
}

func TestReconciler_StartupLogCounts(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	env.seed(t, "Needs a run")
	lifecycletest.SeedReport(t, env.db, lcReport("Checking"), lifecycletest.Spec{State: lifecycle.Verifying})
	lifecycletest.SeedReport(t, env.db, lcReport("Lost plan"), lifecycletest.Spec{State: lifecycle.ExecutingPlan})
	env.svc.StartRecovery()
	eventually(t, "the startup line", func() bool {
		return env.logged("reconciler: startup runs=1 checks=1 plans_lost=1 parked=0")
	})
}

func TestReconciler_DryRunPlanOnEmptyDB(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	env.svc.StartRecovery()
	eventually(t, "the startup line", func() bool {
		return env.logged("reconciler: startup runs=0 checks=0 plans_lost=0 parked=0")
	})
}
