package incident_test

import (
	"context"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/incident"
)

func TestRS3_HoldStaleKeepsRunAlive(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(func(ctx context.Context, gate agent.Gate) agent.RunExit {
		gate.HoldStale(20)
		gate.HoldStale(1)
		<-ctx.Done()
		return agent.RunExit{State: agent.RunSuperseded, Err: ctx.Err()}
	})
	env := newRoutingEnv(t, ag, nil, func(tm *incident.Timers) {
		tm.RetryFirstMinutes, tm.RetrySecondMinutes, tm.StaleRunMinutes = 1, 1, 15
	})
	env.svc.SetUnitForTest(time.Minute)
	id := env.report(t, "Resyncing")
	waitGateStart(t, ag)
	eventually(t, "the held floor", func() bool { return env.svc.StaleFloorForTest(id) == 25 })

	env.svc.RaiseStaleFloorForTest(id, 0, 90)
	if got := env.svc.StaleFloorForTest(id); got != 25 {
		t.Fatalf("an old generation moved the floor to %d", got)
	}

	env.exec(t, `UPDATE incident_events SET at = ? WHERE incident_id = ?`, time.Now().Add(-16*time.Minute), id)
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 0 || len(env.notif.ownerCards()) != 0 {
		t.Fatalf("a held run was judged stale at 16 min: %+v", c)
	}
	env.exec(t, `UPDATE incident_events SET at = ? WHERE incident_id = ?`, time.Now().Add(-26*time.Minute), id)
	if c := env.svc.ReconcileForTest(t.Context()); c.Runs != 1 {
		t.Fatalf("a held run past its floor was not relaunched: %+v", c)
	}
}
