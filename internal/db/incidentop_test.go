package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

func decision(d lifecycle.Decision) db.Decision { return db.Decision{Decision: d} }

func newSpecOp(spec operation.Spec) operation.Op {
	op, err := operation.Next(operation.Op{}, operation.Event{Kind: operation.Created, At: opAt(0), Spec: &spec})
	if err != nil {
		panic(err)
	}
	return op
}

func planState(s *lifecycle.Snapshot, status lifecycle.State, reason lifecycle.Reason, op *operation.Op) {
	s.Status, s.Reason, s.Op = status, reason, op
	s.Plan = nil
	if status == lifecycle.ExecutingPlan || reason == lifecycle.ReasonPlanFailed {
		s.Plan = &lifecycle.Plan{JSON: op.Source, At: time.Now(), FP: card.Fingerprint(op.Source)}
	}
}

func approveWithOp(t *testing.T, d *db.DB, id string, spec operation.Spec) (lifecycle.Snapshot, int64) {
	t.Helper()
	op := newSpecOp(spec)
	a, err := d.ApplyIncident(context.Background(), id, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		planState(&n, lifecycle.ExecutingPlan, lifecycle.ReasonNone, &op)
		return decision(lifecycle.Decision{Next: n, OpWrite: &op, OpNew: true}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a.Snap, a.OpID
}

func pointAt(t *testing.T, d *db.DB, id string, status lifecycle.State, reason lifecycle.Reason, op operation.Op) {
	t.Helper()
	_, err := d.ApplyIncident(context.Background(), id, force(func(s *lifecycle.Snapshot) {
		planState(s, status, reason, &op)
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func seenOp(t *testing.T, d *db.DB, id string) (*operation.Op, bool) {
	t.Helper()
	var got *operation.Op
	var running bool
	_, err := d.ApplyIncident(context.Background(), id, func(cur lifecycle.Snapshot) (db.Decision, error) {
		got, running = cur.Op, cur.In.OpRunning
		return decision(lifecycle.Decision{Next: cur, JournalOnly: true}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, running
}

func lastStatusPayload(t *testing.T, d *db.DB, id string) map[string]any {
	t.Helper()
	events, err := d.EventsSince(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range slices.Backward(events) {
		if e.Kind != "status_changed" {
			continue
		}
		var m map[string]any
		if err = json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	t.Fatal("no status_changed")
	return nil
}

func incidentOpCount(t *testing.T, d *db.DB, id string) int {
	t.Helper()
	n := 0
	for i := int64(1); i < 20; i++ {
		if op, err := d.Operation(context.Background(), i); err == nil && op.Subject.ID == id {
			n++
		}
	}
	return n
}

func TestApplyIncident_ApproveAndOpCommitTogether(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Approve together")
	if err := d.ExecForTest(ctx, `CREATE TRIGGER refuse_op BEFORE INSERT ON operations
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	op := newSpecOp(replaceSpecFor(s.ID))
	_, err := d.ApplyIncident(ctx, s.ID, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		planState(&n, lifecycle.ExecutingPlan, lifecycle.ReasonNone, &op)
		return decision(lifecycle.Decision{Next: n, OpWrite: &op, OpNew: true}), nil
	})
	if err == nil {
		t.Fatal("approve succeeded through a refusing op insert")
	}
	got := snap(t, d, s.ID)
	if got.Status != lifecycle.Investigating || got.Version != s.Version || got.Plan != nil {
		t.Fatalf("incident moved: %+v", got)
	}
}

func TestApplyIncident_ApproveCreatesOpAndLinksIt(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	s := claimed(t, d, "Approve links")
	got, id := approveWithOp(t, d, s.ID, replaceSpecFor(s.ID))
	if id == 0 || got.Op == nil || got.Op.ID != id {
		t.Fatalf("op id %d, snap op %+v", id, got.Op)
	}
	if p := lastStatusPayload(t, d, s.ID); p["op_id"] != float64(id) {
		t.Fatalf("payload %v", p)
	}
	seen, running := seenOp(t, d, s.ID)
	if seen == nil || seen.ID != id || !running {
		t.Fatalf("seen %+v running %v", seen, running)
	}
}

func TestApplyIncident_RefusesForeignSubjectOp(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Foreign op")
	for _, spec := range []operation.Spec{replaceSpecFor("someone-else"), regrabSpecFor(s.ID)} {
		op := newSpecOp(spec)
		_, err := d.ApplyIncident(ctx, s.ID, func(cur lifecycle.Snapshot) (db.Decision, error) {
			n := cur.Clone()
			n.Status = lifecycle.Investigating
			return decision(lifecycle.Decision{Next: n, OpWrite: &op, OpNew: true}), nil
		})
		if !errors.Is(err, lifecycle.ErrIllegal) {
			t.Fatalf("%v: %v", spec.Subject, err)
		}
	}
	if incidentOpCount(t, d, "someone-else")+incidentOpCount(t, d, s.ID) != 0 {
		t.Fatal("a foreign op was written")
	}
}

func TestApplyIncident_PlanLostInterruptsInSameTx(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Plan lost")
	_, id := approveWithOp(t, d, s.ID, replaceSpecFor(s.ID))
	gen := snap(t, d, s.ID).Generation
	a, err := d.ApplyIncident(ctx, s.ID, next(lifecycle.Event{Kind: lifecycle.PlanLost, Gen: gen}))
	if err != nil {
		t.Fatal(err)
	}
	op, err := d.Operation(ctx, id)
	if err != nil || op.Status != operation.FailedAtStep || op.Cause != operation.CauseRestart {
		t.Fatalf("op %+v: %v", op, err)
	}
	got := snap(t, d, s.ID)
	if got.Status != lifecycle.ManualTestNeeded || got.Reason != lifecycle.ReasonPlanFailed {
		t.Fatalf("incident %s/%s", got.Status, got.Reason)
	}
	if a.OpID != id || a.Snap.Op == nil || a.Snap.Op.Version != op.Version {
		t.Fatalf("applied %d %+v", a.OpID, a.Snap.Op)
	}
	if p := lastStatusPayload(t, d, s.ID); p["op_id"] != float64(id) {
		t.Fatalf("payload %v", p)
	}
}

func TestApplyIncident_UniqueRunningOpBackstop(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	s := claimed(t, d, "Backstop")
	got, id := approveWithOp(t, d, s.ID, replaceSpecFor(s.ID))
	second := newSpecOp(replaceSpecFor(s.ID))
	_, err := d.ApplyIncident(context.Background(), s.ID, func(cur lifecycle.Snapshot) (db.Decision, error) {
		if !cur.In.OpRunning {
			t.Error("OpRunning false with a running op")
		}
		n := cur.Clone()
		n.Op = &second
		return decision(lifecycle.Decision{Next: n, OpWrite: &second, OpNew: true}), nil
	})
	if err == nil {
		t.Fatal("a second running op was accepted")
	}
	after := snap(t, d, s.ID)
	if after.Version != got.Version {
		t.Fatalf("version %d to %d", got.Version, after.Version)
	}
	if seen, _ := seenOp(t, d, s.ID); seen == nil || seen.ID != id {
		t.Fatalf("seen %+v", seen)
	}
}

func TestSnapshotOp_FromLatestStatusChanged(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	s := claimed(t, d, "Latest op")
	if seen, running := seenOp(t, d, s.ID); seen != nil || running {
		t.Fatalf("before: %+v %v", seen, running)
	}
	_, id := approveWithOp(t, d, s.ID, replaceSpecFor(s.ID))
	seen, running := seenOp(t, d, s.ID)
	if seen == nil || seen.ID != id || seen.Status != operation.Running || !running {
		t.Fatalf("seen %+v %v", seen, running)
	}
	finishDone(t, d, id)
	seen, running = seenOp(t, d, s.ID)
	if seen == nil || seen.Status != operation.Done || running {
		t.Fatalf("after finish %+v %v", seen, running)
	}
}

func TestSnapshotOp_P3EraEventIsNil(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "P3 era")
	approveWithOp(t, d, s.ID, replaceSpecFor(s.ID))
	err := d.ExecForTest(ctx, `INSERT INTO incident_events (incident_id, at, kind, payload)
		VALUES (?, ?, 'status_changed', '{"status":"executing_plan","version":99}')`, s.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if seen, _ := seenOp(t, d, s.ID); seen != nil {
		t.Fatalf("loaded %+v", seen)
	}
}

func TestSnapshotOp_ForeignOrMissingIDIsNil(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Missing op")
	approveWithOp(t, d, s.ID, replaceSpecFor(s.ID))
	other := newOp(t, d, replaceSpecFor("someone-else"))
	for _, id := range []int64{999, other.ID} {
		err := d.ExecForTest(ctx, `INSERT INTO incident_events (incident_id, at, kind, payload)
			VALUES (?, ?, 'status_changed', json_object('status', 'executing_plan', 'version', 98, 'op_id', ?))`,
			s.ID, time.Now(), id)
		if err != nil {
			t.Fatal(err)
		}
		if seen, _ := seenOp(t, d, s.ID); seen != nil {
			t.Fatalf("op %d loaded %+v", id, seen)
		}
	}
}

func TestSnapshotOp_SourceMismatchIsNil(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	s := claimed(t, d, "Source mismatch")
	approveWithOp(t, d, s.ID, replaceSpecFor(s.ID))
	if seen, _ := seenOp(t, d, s.ID); seen == nil {
		t.Fatal("op not loaded before the plan changed")
	}
	if err := d.ExecForTest(context.Background(),
		`UPDATE incidents SET escalation_plan = '{"plan":2}' WHERE id = ?`, s.ID); err != nil {
		t.Fatal(err)
	}
	if seen, _ := seenOp(t, d, s.ID); seen != nil {
		t.Fatalf("loaded %+v", seen)
	}
}

func TestApprove_BuildErrorShowsNoOldOpLine(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	s := claimed(t, d, "Build error")
	old := newOp(t, d, replaceSpecFor(s.ID))
	old = step(t, d, old.ID, startEv(0), finishEv(0, operation.ResultFailed))
	if old.Status != operation.FailedAtStep {
		t.Fatalf("setup: %+v", old)
	}
	pointAt(t, d, s.ID, lifecycle.ManualTestNeeded, lifecycle.ReasonPlanFailed, old)
	cur := snap(t, d, s.ID)
	if seen, _ := seenOp(t, d, s.ID); seen == nil || seen.ID != old.ID {
		t.Fatalf("setup: %+v", seen)
	}
	a, err := d.ApplyIncident(context.Background(), s.ID, next(lifecycle.Event{
		Kind: lifecycle.OwnerApprove, Version: cur.Version, HasVersion: true, PlanFP: cur.Plan.FP,
		BuildErr: "no such file",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if a.Snap.Reason != lifecycle.ReasonPlanFailed || a.Snap.Op != nil || a.OpID != 0 {
		t.Fatalf("applied %s op %+v id %d", a.Snap.Reason, a.Snap.Op, a.OpID)
	}
	if p := lastStatusPayload(t, d, s.ID); p["op_id"] != nil {
		t.Fatalf("payload %v", p)
	}
	if seen, _ := seenOp(t, d, s.ID); seen != nil {
		t.Fatalf("old op loaded: %+v", seen)
	}
}

func TestSnapshotOp_OutsideStatusChangedKeepsOp(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Outside writers")
	op := newOp(t, d, replaceSpecFor(s.ID))
	op = step(t, d, op.ID, startEv(0), finishEv(0, operation.ResultDone))
	op = step(t, d, op.ID, startEv(1), finishEv(1, operation.ResultFailed))
	pointAt(t, d, s.ID, lifecycle.ManualTestNeeded, lifecycle.ReasonPlanPartial, op)
	if err := d.SetSetting(ctx, "autonomous_paused", "true"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSetting(ctx, "autonomous_paused", "false"); err != nil {
		t.Fatal(err)
	}
	if seen, _ := seenOp(t, d, s.ID); seen == nil || seen.ID != op.ID {
		t.Fatalf("seen %+v", seen)
	}
}
