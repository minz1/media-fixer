package incident_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestI1_17_HandleSystemicOneWrite(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	for i := range 5 {
		env.seed(t, fmt.Sprintf("Busy %d", i))
	}
	id := env.report(t, "Systemic")
	s := env.snap(t, id)
	if s.Status != lifecycle.Blocked || !s.Locked {
		t.Fatalf("systemic report stored as %s locked=%v", s.Status, s.Locked)
	}
	if kinds := env.kinds(t, id); !slices.Equal(kinds, []string{"incident_created", "reporter_added"}) {
		t.Fatalf("events %v, want one insert with no status write after it", kinds)
	}
	eventually(t, "the systemic alert", func() bool { return len(env.notif.ownerCards()) == 1 })
}

func TestI1_18_HandleConcurrentMerge(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	ids := make(chan string, 6)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			inc, err := env.svc.Handle(t.Context(), &incident.Report{
				Source: "discord", ReportedBy: fmt.Sprintf("user-%d", i), ReporterDiscordID: fmt.Sprintf("u-%d", i),
				What: "cant_play", Title: "Concurrent (2025)",
			})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- inc.ID
		})
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("%d incidents for one title", len(seen))
	}
}

func TestI4_9_PausedNewIncidentParks(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	if err := env.svc.SetAutonomousPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	id := env.report(t, "Paused report")
	if s := env.snap(t, id); s.Status != lifecycle.Open || !s.Parked || s.Generation != 0 {
		t.Fatalf("paused report stored as %s parked=%v gen=%d", s.Status, s.Parked, s.Generation)
	}
	if env.svc.ReservedOrRunningForTest(id) {
		t.Fatal("a paused report launched a run")
	}
}

func TestHandle_ExecutingPlanAbsorbsReport(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.Seed(t, env.db, lifecycletest.Spec{Title: "Running Plan", State: lifecycle.ExecutingPlan})
	id := env.report(t, "Running Plan")
	if id != s.ID {
		t.Fatalf("a report for an incident running its plan opened %s", id)
	}
	if got := env.snap(t, id); got.Status != lifecycle.ExecutingPlan {
		t.Fatalf("status %s", got.Status)
	}
}

func TestHandle_SeerrCommentOnce(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	seerr := &fakeSeerr{}
	env.svc.SetSeerr(seerr)
	for range 2 {
		if _, err := env.svc.Handle(t.Context(), seerrReport("9")); err != nil {
			t.Fatal(err)
		}
	}
	seerr.mu.Lock()
	defer seerr.mu.Unlock()
	if n := len(seerr.comments["9"]); n != 1 {
		t.Fatalf("%d comments on one issue", n)
	}
}

func TestHandle_ReservesBeforeApply(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	stop := make(chan struct{})
	var bad atomic.Value
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := env.db.ListDriven(context.Background())
			if err != nil {
				continue
			}
			for _, r := range rows {
				if r.Snap.Status == lifecycle.Open && !env.svc.ReservedOrRunningForTest(r.Snap.ID) {
					bad.Store(r.Snap.ID)
				}
			}
		}
	})
	for i := range 4 {
		env.report(t, fmt.Sprintf("Reserved %d", i))
	}
	waitGateStart(t, ag)
	close(stop)
	wg.Wait()
	if id := bad.Load(); id != nil {
		t.Fatalf("%v was visible open with neither a reservation nor a run", id)
	}
}
