package incident_test

import (
	"encoding/json"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
)

func eventsOfKind(t *testing.T, d *db.DB, id string, kind journal.Kind) []*db.Event {
	t.Helper()
	all, err := d.EventsSince(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []*db.Event
	for _, e := range all {
		if e.Kind == string(kind) {
			out = append(out, e)
		}
	}
	return out
}

func TestGate_AllowedActAppendsAttemptInSameTx(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.claimed(t, "Attempt")
	g := env.svc.GateForTest(s.ID, s.Generation)

	v, err := g.Act(t.Context(), "restart_jellyfin", map[string]any{"item_id": "a"})
	if err != nil || v != agent.GateAllowed {
		t.Fatalf("allowed act: %v, %v", v, err)
	}
	got := eventsOfKind(t, env.db, s.ID, journal.KindActionAttempted)
	if n := env.snap(t, s.ID).ActionCount; n != 1 || len(got) != 1 {
		t.Fatalf("count %d, %d attempt events, want 1 and 1", n, len(got))
	}
	var p struct {
		Generation int64          `json:"generation"`
		Tool       string         `json:"tool"`
		Params     map[string]any `json:"params"`
	}
	if err = json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Generation != s.Generation || p.Tool != "restart_jellyfin" || p.Params["item_id"] != "a" {
		t.Fatalf("payload %+v", p)
	}

	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.Locked = true })
	if v, err = g.Act(t.Context(), "restart_jellyfin", nil); err != nil || v != agent.GateDiagnosisOnly {
		t.Fatalf("locked act: %v, %v", v, err)
	}
	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.Locked, n.ActionCount = false, agent.MaxAutonomousActions })
	if v, err = g.Act(t.Context(), "restart_jellyfin", nil); err != nil || v != agent.GateTripped {
		t.Fatalf("tripping act: %v, %v", v, err)
	}
	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.Locked, n.ActionCount = false, 0 })
	if v, err = env.svc.GateForTest(s.ID, s.Generation+9).Act(t.Context(), "restart_jellyfin", nil); err != nil ||
		v != agent.GateSuperseded {
		t.Fatalf("stale act: %v, %v", v, err)
	}
	if n := len(eventsOfKind(t, env.db, s.ID, journal.KindActionAttempted)); n != 1 {
		t.Fatalf("%d attempt events after the refused acts, want still 1", n)
	}
	if n := env.snap(t, s.ID).ActionCount; n != 0 {
		t.Fatalf("count %d after the refused acts, want 0", n)
	}
}

func ownerRerunGens(t *testing.T, env *lcEnv, id string) []int64 {
	t.Helper()
	var out []int64
	for _, e := range eventsOfKind(t, env.db, id, journal.KindOwnerRerun) {
		var p struct {
			Generation int64 `json:"generation"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Generation)
	}
	return out
}

func TestOwnerRerun_AppendsMarker(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	ctx := t.Context()
	seed := func(title string) lifecycle.Snapshot {
		return lifecycletest.SeedReport(t, env.db, lcReport(title), lifecycletest.Spec{State: lifecycle.AgentFixed})
	}

	live := seed("Live rerun")
	if err := env.svc.Rerun(ctx, live.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	if got, want := ownerRerunGens(t, env, live.ID), env.snap(t, live.ID).Generation; len(got) != 1 || got[0] != want {
		t.Fatalf("live rerun markers %v, want [%d]", got, want)
	}

	sys := seed("System rerun")
	if err := env.svc.SystemRerun(ctx, sys.ID, sys.Generation); err != nil {
		t.Fatal(err)
	}
	pending := env.seed(t, "Relaunched")
	env.svc.ReconcileForTest(ctx)
	for _, id := range []string{sys.ID, pending.ID} {
		if got := ownerRerunGens(t, env, id); len(got) != 0 {
			t.Fatalf("incident %s: markers %v, want none", id, got)
		}
	}

	parked := seed("Paused rerun")
	if err := env.svc.SetAutonomousPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Rerun(ctx, parked.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	after := env.snap(t, parked.ID)
	if got := ownerRerunGens(t, env, parked.ID); !after.Parked || len(got) != 1 || got[0] != after.Generation {
		t.Fatalf("paused rerun markers %v, want [%d], parked %v", got, after.Generation, after.Parked)
	}
}
