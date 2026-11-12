package incident_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func TestRunManager_SupersedeCancelsLowerGen(t *testing.T) {
	t.Parallel()
	m := incident.NewRunManagerForTest(t.Context())
	first, ok := m.SupersedeGen("inc", 1)
	if !ok {
		t.Fatal("first launch refused")
	}
	if _, ok = m.SupersedeGen("inc", 1); ok {
		t.Fatal("a second worker for the same generation was registered")
	}
	second, ok := m.SupersedeGen("inc", 2)
	if !ok {
		t.Fatal("a newer generation was refused")
	}
	if first.Err() == nil {
		t.Fatal("the lower generation kept running")
	}
	if _, ok = m.SupersedeGen("inc", 1); ok {
		t.Fatal("an older generation replaced a newer one")
	}
	m.CancelBelow("inc", 3)
	if second.Err() == nil {
		t.Fatal("CancelRun left the lower generation running")
	}
}

func TestRunManager_ReserveHidesFromReconciler(t *testing.T) {
	t.Parallel()
	m := incident.NewRunManagerForTest(t.Context())
	row := []lifecycle.Observed{{Snap: lifecycle.Snapshot{ID: "inc", Status: lifecycle.Open, Generation: 1}}}
	plan := func() []lifecycle.Intent {
		return lifecycle.Reconcile(row, m.Liveness(), lifecycle.Policy{Now: time.Now(), StaleAfter: time.Hour})
	}
	first, second := m.Reserve("inc"), m.Reserve("inc")
	if got := plan(); len(got) != 0 {
		t.Fatalf("reserved incident planned %+v", got)
	}
	first()
	first()
	if got := plan(); len(got) != 0 {
		t.Fatalf("a second reserver was wiped by the first's release: %+v", got)
	}
	second()
	if got := plan(); len(got) != 1 || got[0].Event.Kind != lifecycle.Relaunch {
		t.Fatalf("released incident planned %+v", got)
	}
}

func TestChecks_TimerArmedOncePerGeneration(t *testing.T) {
	t.Parallel()
	m := incident.NewRunManagerForTest(t.Context())
	var fired atomic.Int64
	later := time.Now().Add(time.Hour)
	if !m.ArmCheck("inc", 1, later, func() { fired.Add(1) }) {
		t.Fatal("first arm refused")
	}
	if m.ArmCheck("inc", 1, later, func() { fired.Add(1) }) {
		t.Fatal("armed twice for one generation")
	}
	if !m.ArmCheck("inc", 2, time.Now(), func() { fired.Add(10) }) {
		t.Fatal("a newer generation was refused")
	}
	eventually(t, "the newer timer to fire", func() bool { return fired.Load() == 10 })
	if got := m.Liveness().Armed["inc"]; got != 0 {
		t.Fatalf("a fired timer stays armed for gen %d", got)
	}
	if m.ArmCheck("inc", 1, time.Now(), func() { fired.Add(100) }) {
		t.Fatal("an older generation armed after a newer one")
	}
}

func TestChecks_RefusedAfterStopLogsInfo(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.seed(t, "Stopped check")
	inc, err := env.db.GetIncident(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.svc.Stop()
	env.svc.ScheduleCheckForTest(inc, s.Generation, time.Now())
	eventually(t, "the refusal log line", func() bool {
		return env.logged("level=INFO msg=\"check not started, shutting down\"")
	})
}

func TestEffects_RunOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	ctx := context.Background()
	s := env.claimed(t, "After commit")
	env.exec(t, `CREATE TRIGGER refuse BEFORE UPDATE OF status ON incidents
		BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	if _, err := env.svc.ApplyForTest(ctx, s.ID, lifecycle.Event{Kind: lifecycle.OwnerResolve}); err == nil {
		t.Fatal("resolve committed through a refusing trigger")
	}
	if got := env.notif.userCards(); len(got) != 0 {
		t.Fatalf("effects ran for a rolled-back Apply: %+v", got)
	}
	env.exec(t, `DROP TRIGGER refuse`)
	if _, err := env.svc.ApplyForTest(ctx, s.ID, lifecycle.Event{Kind: lifecycle.OwnerResolve}); err != nil {
		t.Fatal(err)
	}
	if got := env.notif.userCards(); len(got) != 1 || !strings.Contains(got[0].Plain(), "has been resolved") {
		t.Fatalf("reporter cards after commit: %+v", got)
	}
}

func TestEffects_FailureLoggedRestStillRun(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	ctx := context.Background()
	s := env.claimed(t, "Owner down")
	env.notif.mu.Lock()
	env.notif.ownerFails = 1
	env.notif.mu.Unlock()
	ev := lifecycle.Event{
		Kind: lifecycle.RunFinished, Gen: s.Generation,
		Outcome: lifecycle.Outcome{Kind: lifecycle.OutcomeEscalate, Reason: lifecycle.ReasonNeedsOwner},
		Notice:  lifecycle.Notice{Kind: lifecycle.NoticeCard, Title: "needs you"},
	}
	if _, err := env.svc.ApplyForTest(ctx, s.ID, ev); err != nil {
		t.Fatal(err)
	}
	if !env.logged("msg=\"outbox send failed\"") {
		t.Fatalf("owner DM failure not logged: %s", env.logs.String())
	}
	if got := env.notif.userCards(); len(got) != 1 || !strings.Contains(got[0].Plain(), "Handed") {
		t.Fatalf("reporter note after a failed owner DM: %+v", got)
	}
	if got := env.snap(t, s.ID); got.Status != lifecycle.ManualTestNeeded {
		t.Fatalf("status %s", got.Status)
	}
}
