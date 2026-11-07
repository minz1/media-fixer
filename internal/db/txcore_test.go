package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func findingRoot(t *testing.T, d *db.DB, id string) string {
	t.Helper()
	got, err := d.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := got.Finding.(map[string]any)
	root, _ := m["root_cause"].(string)
	return root
}

func eventKinds(t *testing.T, d *db.DB, id string) []string {
	t.Helper()
	events, err := d.EventsSince(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(events))
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func TestAppendEvents_ActionLogRowCommitsWithEvent(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id := newClaimIncident(t, d)

	e := &db.Event{
		IncidentID: id, Kind: "action_applied", Payload: json.RawMessage(`{"id":"a-1"}`),
		ActionLog: &db.ActionLog{
			ID: "a-1", IncidentID: id, Action: "refresh_links", TriggeredBy: "agent", Status: db.ActionApplied,
		},
	}
	if err := d.AppendEvents(ctx, e); err != nil {
		t.Fatal(err)
	}
	if e.Seq == 0 {
		t.Error("seq not set")
	}
	actions, err := d.ListActions(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].ID != "a-1" || actions[0].Action != "refresh_links" {
		t.Fatalf("actions = %+v, want one refresh_links row with the event's id", actions)
	}
}

func TestAppendEvents_RollsBackAll(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id := newClaimIncident(t, d)
	base := len(eventKinds(t, d, id))

	first := &db.Event{IncidentID: id, Kind: "status_changed", Payload: json.RawMessage(`{}`)}
	second := &db.Event{IncidentID: "no-such-incident", Kind: "status_changed", Payload: json.RawMessage(`{}`)}
	if err := d.AppendEvents(ctx, first, second); err == nil {
		t.Fatal("append with an unknown incident succeeded")
	}
	if kinds := eventKinds(t, d, id); len(kinds) != base {
		t.Fatalf("events after rollback = %v, want the %d fixture events only", kinds, base)
	}
}

func attemptFinished(finding json.RawMessage, e *db.Event) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		ev := lifecycle.Event{Kind: lifecycle.AttemptFinished, Gen: cur.Generation}
		if len(finding) > 0 {
			ev.Finding = &lifecycle.Finding{JSON: finding}
		}
		dec, err := next(ev)(cur)
		dec.Events = []*db.Event{e}
		return dec, err
	}
}

func TestAttemptFinished_FindingAndRunFinishedCommitTogether(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Commit together")
	bad := &db.Event{IncidentID: "no-such-incident", Kind: "run_finished", Payload: json.RawMessage(`{}`)}
	if _, err := d.ApplyIncident(ctx, s.ID, attemptFinished(json.RawMessage(`{"root_cause":"new"}`), bad)); err == nil {
		t.Fatal("an attempt with an unknown event incident succeeded")
	}
	if root := findingRoot(t, d, s.ID); root != "" {
		t.Fatalf("finding after a failed attempt = %q, want none", root)
	}
	good := &db.Event{IncidentID: s.ID, Kind: "run_finished", Payload: json.RawMessage(`{}`)}
	if _, err := d.ApplyIncident(
		ctx,
		s.ID,
		attemptFinished(json.RawMessage(`{"root_cause":"new"}`), good),
	); err != nil {
		t.Fatal(err)
	}
	if root := findingRoot(t, d, s.ID); root != "new" {
		t.Fatalf("finding after the attempt = %q, want new", root)
	}
	got, err := d.GetIncident(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ra, _ := got.RecommendedActions.(map[string]any); ra["root_cause"] != "new" {
		t.Errorf("recommended_actions = %v, want the finding", got.RecommendedActions)
	}
	if good.Seq == 0 {
		t.Fatal("run_finished has no seq")
	}
}

func TestAttemptFinished_NoFindingLeavesFindingUntouched(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Keeps finding")
	keep := &db.Event{IncidentID: s.ID, Kind: "run_finished", Payload: json.RawMessage(`{}`)}
	if _, err := d.ApplyIncident(
		ctx,
		s.ID,
		attemptFinished(json.RawMessage(`{"root_cause":"kept"}`), keep),
	); err != nil {
		t.Fatal(err)
	}
	e := &db.Event{IncidentID: s.ID, Kind: "run_finished", Payload: json.RawMessage(`{}`)}
	if _, err := d.ApplyIncident(ctx, s.ID, attemptFinished(nil, e)); err != nil {
		t.Fatal(err)
	}
	if root := findingRoot(t, d, s.ID); root != "kept" {
		t.Fatalf("finding = %q, want kept", root)
	}
}

func TestAppendEvents_RollbackLeavesSeqUnset(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	id := newClaimIncident(t, d)

	first := &db.Event{IncidentID: id, Kind: "status_changed", Payload: json.RawMessage(`{}`)}
	second := &db.Event{IncidentID: "no-such-incident", Kind: "status_changed", Payload: json.RawMessage(`{}`)}
	if err := d.AppendEvents(context.Background(), first, second); err == nil {
		t.Fatal("append with an unknown incident succeeded")
	}
	if first.Seq != 0 || second.Seq != 0 {
		t.Errorf("seqs after rollback = %d, %d, want 0, 0", first.Seq, second.Seq)
	}
}
