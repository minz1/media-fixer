package incident_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestI1_13_ServiceUnlockBlockedLaunches(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Blocked show"), lifecycletest.Spec{State: lifecycle.Blocked})
	if err := env.svc.Unlock(t.Context(), s.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	waitGateStart(t, ag)
	if got := env.snap(t, s.ID); got.Status != lifecycle.Investigating || got.Locked || got.ActionCount != 0 {
		t.Fatalf("after unlock: %s locked=%v count=%d", got.Status, got.Locked, got.ActionCount)
	}
}

func TestI1_20_RerunNeverCancelsWithoutReplacement(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Rerun fails")
	waitGateStart(t, ag)
	env.exec(t, `CREATE TRIGGER refuse BEFORE UPDATE OF status ON incidents BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	if err := env.svc.Rerun(t.Context(), id, env.svc.CurrentVersion(t.Context(), id)); err == nil {
		t.Fatal("rerun succeeded through a refusing trigger")
	}
	if !env.svc.ReservedOrRunningForTest(id) || ag.runs.Load() != 1 {
		t.Fatal("a failed rerun cancelled the running run")
	}
	if got := env.snap(t, id); got.Status != lifecycle.Investigating {
		t.Fatalf("status %s", got.Status)
	}
}

func TestI5_10_RerunNoReplacementImpossible(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Rerun replaces")
	waitGateStart(t, ag)
	if err := env.svc.Rerun(t.Context(), id, env.svc.CurrentVersion(t.Context(), id)); err != nil {
		t.Fatal(err)
	}
	waitGateStart(t, ag)
	s := env.snap(t, id)
	if s.Status != lifecycle.Investigating || env.svc.ArmedForTest(id) != 0 || ag.runs.Load() != 2 {
		t.Fatalf("after a rerun: %s, %d runs", s.Status, ag.runs.Load())
	}
}

func TestI2_13_KeepSearchingOneTransaction(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Keep one"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	before := env.count(t, s.ID, "status_changed")
	if err := env.svc.KeepSearching(t.Context(), s.ID, env.version(t, s.ID)); err != nil {
		t.Fatal(err)
	}
	got := env.snap(t, s.ID)
	if got.Status != lifecycle.AwaitingDownload || got.Download == nil || !got.Download.KeepSearching ||
		got.NextCheck.IsZero() || got.Reason != lifecycle.ReasonNone {
		t.Fatalf("after keep searching: %+v", got)
	}
	if n := env.count(t, s.ID, "status_changed") - before; n != 1 {
		t.Fatalf("%d status_changed events, want one write", n)
	}
	eventually(t, "the download check", func() bool { return env.svc.ArmedForTest(s.ID) == got.Generation })
}

func TestI2_14_RerunClearsPendingOutcome(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(
		t,
		env.db,
		lcReport("Rerun clears"),
		lifecycletest.Spec{State: lifecycle.AwaitingDownload},
	)
	if err := env.svc.Rerun(t.Context(), s.ID, env.svc.CurrentVersion(t.Context(), s.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.GetPendingOutcome(t.Context(), s.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("pending outcome after rerun: %v", err)
	}
}

func TestI2_15_ResolveClearsPendingOutcome(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(
		t,
		env.db,
		lcReport("Resolve clears"),
		lifecycletest.Spec{State: lifecycle.AwaitingDownload},
	)
	if err := env.svc.Resolve(t.Context(), s.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.GetPendingOutcome(t.Context(), s.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("pending outcome after resolve: %v", err)
	}
}

func TestA1_8_ServiceRerunResetsBudget(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.claimed(t, "Rerun budget")
	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.Locked, n.ActionCount = true, 3 })
	if err := env.svc.Rerun(t.Context(), s.ID, env.svc.CurrentVersion(t.Context(), s.ID)); err != nil {
		t.Fatal(err)
	}
	if got := env.snap(t, s.ID); got.Locked || got.ActionCount != 0 {
		t.Fatalf("after an owner rerun: locked=%v count=%d", got.Locked, got.ActionCount)
	}
}

func TestA1_9_ServiceUnlockResetsBudget(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.claimed(t, "Unlock budget")
	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.Locked, n.ActionCount = true, 3 })
	if err := env.svc.Unlock(t.Context(), s.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	if got := env.snap(t, s.ID); got.Locked || got.ActionCount != 0 || got.Status != lifecycle.Investigating {
		t.Fatalf("after unlock: %s locked=%v count=%d", got.Status, got.Locked, got.ActionCount)
	}
}

func TestRerun_PausedParksAndNotesReporters(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Paused rerun"), lifecycletest.Spec{State: lifecycle.AgentFixed})
	if err := env.svc.SetAutonomousPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Rerun(t.Context(), s.ID, env.svc.CurrentVersion(t.Context(), s.ID)); err != nil {
		t.Fatal(err)
	}
	if got := env.snap(t, s.ID); got.Status != lifecycle.Reopened || !got.Parked {
		t.Fatalf("paused rerun: %s parked=%v", got.Status, got.Parked)
	}
	cards := env.notif.userCards()
	if len(cards) != 1 || !strings.Contains(cards[0].Plain(), "Taking another look") {
		t.Fatalf("reporter cards %+v", cards)
	}
	if ag.runs.Load() != 0 {
		t.Fatal("a paused rerun ran")
	}
}

func TestResolve_MidRunNotRefusedForVersion(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.claimed(t, "Resolve mid run")
	old := env.version(t, s.ID)
	if _, err := env.svc.ApplyForTest(t.Context(), s.ID, lifecycle.Event{
		Kind: lifecycle.ActionAttempted, Gen: s.Generation, Tool: agent.ToolArrSearchMissing,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Resolve(t.Context(), s.ID, old); err != nil {
		t.Fatalf("resolve with the version from before the action: %v", err)
	}
	if got := env.snap(t, s.ID); got.Status != lifecycle.Resolved {
		t.Fatalf("status %s", got.Status)
	}
}

func TestKeepSearching_StaleVersionRefused(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Keep stale"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	stale := incident.Version{N: env.snap(t, s.ID).Version - 1, Set: true}
	if err := env.svc.KeepSearching(t.Context(), s.ID, stale); !errors.Is(err, lifecycle.ErrChanged) {
		t.Fatalf("stale keep searching: %v", err)
	}
	if got := env.snap(t, s.ID); got.Status != lifecycle.ManualTestNeeded {
		t.Fatalf("status %s", got.Status)
	}
}

func TestOwnerEvent_RestartingBeforeVersion(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Restarting"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	env.svc.Stop()
	stale := incident.Version{N: env.snap(t, s.ID).Version - 1, Set: true}
	if err := env.svc.KeepSearching(context.Background(), s.ID, stale); !errors.Is(err, card.ErrRestarting) {
		t.Fatalf("keep searching while restarting: %v", err)
	}
}

func TestF21_DoubleRerunLaunchesOnce(t *testing.T) {
	t.Parallel()
	for _, st := range []lifecycle.State{lifecycle.ManualTestNeeded, lifecycle.Reopened} {
		env := newLifecycleEnv(t, nil)
		s := lifecycletest.SeedReport(t, env.db, lcReport("Double rerun"), lifecycletest.Spec{State: st})
		v := env.svc.CurrentVersion(t.Context(), s.ID)
		if err := env.svc.Rerun(t.Context(), s.ID, v); err != nil {
			t.Fatal(err)
		}
		err := env.svc.Rerun(t.Context(), s.ID, v)
		if got := env.snap(t, s.ID).Generation - s.Generation; !incident.IsRefusal(err) || got != 1 {
			t.Fatalf("%s, second press: err %v, generation moved by %d", st, err, got)
		}
	}
}
