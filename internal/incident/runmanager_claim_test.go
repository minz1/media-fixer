package incident_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/incident"
)

func TestClaimCheck_StaleClaimKeepsNewerInflight(t *testing.T) {
	t.Parallel()
	svc := newLifecycleEnv(t, nil).svc
	doneNew := svc.ClaimCheckForTest("inc", 6)
	doneOld := svc.ClaimCheckForTest("inc", 5)
	if got := svc.ArmedForTest("inc"); got != 6 {
		t.Fatalf("armed gen after a stale claim = %d, want 6", got)
	}
	doneOld()
	if got := svc.ArmedForTest("inc"); got != 6 {
		t.Fatalf("armed gen after the stale check finished = %d, want 6", got)
	}
	doneNew()
	if got := svc.ArmedForTest("inc"); got != 0 {
		t.Fatalf("armed gen after both finished = %d, want none", got)
	}
}

func TestE7_RunEndWakesReconciler(t *testing.T) {
	t.Parallel()
	m := incident.NewRunManagerForTest(t.Context())
	woke := 0
	m.SetWake(func() { woke++ })
	current := m.End("inc")
	current()
	if woke != 1 {
		t.Fatalf("a still-current run end woke the reconciler %d times, want 1", woke)
	}
	superseding := m.End("inc")
	current()
	if woke != 1 {
		t.Fatalf("a stale run end woke the reconciler (%d wakes, want 1)", woke)
	}
	superseding()
	if woke != 2 {
		t.Fatalf("the current run end woke the reconciler %d times, want 2", woke)
	}
}

func TestE7_PlanReleaseWakesReconciler(t *testing.T) {
	t.Parallel()
	m := incident.NewRunManagerForTest(t.Context())
	woke := 0
	m.SetWake(func() { woke++ })
	first := m.PlanStarted("inc")
	second := m.PlanStarted("inc")
	first()
	if woke != 0 {
		t.Fatalf("a plan release above zero woke the reconciler (%d wakes, want 0)", woke)
	}
	second()
	if woke != 1 {
		t.Fatalf("the plan release to zero woke the reconciler %d times, want 1", woke)
	}
}
