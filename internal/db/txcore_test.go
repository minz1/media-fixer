package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
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

	first := &db.Event{IncidentID: id, Kind: "status_changed", Payload: json.RawMessage(`{}`)}
	second := &db.Event{IncidentID: "no-such-incident", Kind: "status_changed", Payload: json.RawMessage(`{}`)}
	if err := d.AppendEvents(ctx, first, second); err == nil {
		t.Fatal("append with an unknown incident succeeded")
	}
	if kinds := eventKinds(t, d, id); len(kinds) != 0 {
		t.Fatalf("events after rollback = %v, want none", kinds)
	}
}

func TestRecordRunExit_FindingAndEventCommitTogether(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id := newClaimIncident(t, d)
	if err := d.SetIncidentFinding(ctx, id, map[string]any{"root_cause": "old"}, nil); err != nil {
		t.Fatal(err)
	}

	bad := &db.Event{IncidentID: "no-such-incident", Kind: "run_finished", Payload: json.RawMessage(`{}`)}
	if err := d.RecordRunExit(ctx, id, json.RawMessage(`{"root_cause":"new"}`), bad); err == nil {
		t.Fatal("run exit with an unknown event incident succeeded")
	}
	if root := findingRoot(t, d, id); root != "old" {
		t.Fatalf("finding after a failed exit = %q, want old", root)
	}

	good := &db.Event{IncidentID: id, Kind: "run_finished", Payload: json.RawMessage(`{}`)}
	if err := d.RecordRunExit(ctx, id, json.RawMessage(`{"root_cause":"new"}`), good); err != nil {
		t.Fatal(err)
	}
	if root := findingRoot(t, d, id); root != "new" {
		t.Fatalf("finding after the exit = %q, want new", root)
	}
	got, err := d.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ra, _ := got.RecommendedActions.(map[string]any); ra["root_cause"] != "new" {
		t.Errorf("recommended_actions = %v, want the finding", got.RecommendedActions)
	}
	if kinds := eventKinds(t, d, id); len(kinds) != 1 || kinds[0] != "run_finished" || good.Seq == 0 {
		t.Fatalf("events = %v (seq %d), want one run_finished", kinds, good.Seq)
	}
}

func TestRecordRunExit_NilFindingLeavesFindingUntouched(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id := newClaimIncident(t, d)
	if err := d.SetIncidentFinding(ctx, id, map[string]any{"root_cause": "kept"}, nil); err != nil {
		t.Fatal(err)
	}

	for _, finding := range []json.RawMessage{nil, json.RawMessage(`null`)} {
		e := &db.Event{IncidentID: id, Kind: "run_finished", Payload: json.RawMessage(`{}`)}
		if err := d.RecordRunExit(ctx, id, finding, e); err != nil {
			t.Fatal(err)
		}
	}
	if root := findingRoot(t, d, id); root != "kept" {
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
