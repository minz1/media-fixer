package incident_test

import (
	"sync/atomic"
	"testing"
	"time"

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

func TestF1_EarlierArmDuringInflightCheckRunsOnce(t *testing.T) {
	t.Parallel()
	m := incident.NewRunManagerForTest(t.Context())
	release := make(chan struct{})
	var runs atomic.Int32
	fire := func() { runs.Add(1); <-release }
	m.ArmCheck("i1", 3, time.Now(), fire)
	deadline := time.Now().Add(5 * time.Second)
	for runs.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.ArmCheckEarlier("i1", 3, time.Now(), fire) {
		t.Fatal("an earlier arm at the in-flight generation was accepted")
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("checks run = %d, want 1", got)
	}
}

func TestF32_StaleEarlyArmKeepsNewerSeqTimer(t *testing.T) {
	t.Parallel()
	m := incident.NewRunManagerForTest(t.Context())
	var fired atomic.Int32
	later := time.Now().Add(time.Hour)
	if !m.ArmCheckSeq("inc", 3, 2, later, false, func() { fired.Add(1) }) {
		t.Fatal("arming seq 2 was refused")
	}
	if m.ArmCheckSeq("inc", 3, 1, time.Now(), true, func() { fired.Add(1) }) {
		t.Fatal("an early arm carrying the older seq 1 displaced the live seq 2 timer")
	}
	if at, ok := m.ArmedAt("inc"); !ok || !at.Equal(later) {
		t.Fatalf("armed at %v (armed %v), want the seq 2 timer at %v", at, ok, later)
	}
	if !m.ArmCheckSeq("inc", 3, 2, time.Now().Add(time.Minute), true, func() {}) {
		t.Fatal("an early arm with the same seq was refused")
	}
	if !m.ArmCheckSeq("inc", 3, 3, time.Now().Add(2*time.Hour), false, func() {}) {
		t.Fatal("an arm with a newer seq was refused")
	}
	if m.ArmCheckSeq("inc", 3, 2, time.Now().Add(3*time.Hour), false, func() {}) {
		t.Fatal("a later arm with an older seq replaced the newer seq timer")
	}
	if fired.Load() != 0 {
		t.Fatalf("a stale arm fired %d times", fired.Load())
	}
}
