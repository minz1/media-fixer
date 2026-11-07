package journal_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

type exitPayload struct {
	Rounds int    `json:"rounds"`
	Error  string `json:"error"`
	Exit   string `json:"exit"`
}

func lastEvent(t *testing.T, j *journal.Journal, id string) *db.Event {
	t.Helper()
	events, err := j.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	return events[len(events)-1]
}

func TestAppend_FailureNotifiesNothing(t *testing.T) {
	t.Parallel()
	j, _ := openTestJournal(t)
	ch, unsubscribe := j.Subscribe("no-such-incident")
	defer unsubscribe()
	all, unsubscribeAll := j.SubscribeAll()
	defer unsubscribeAll()

	err := j.Append(context.Background(), &db.Event{
		IncidentID: "no-such-incident", Kind: string(journal.KindStatusChanged), Payload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("append to an unknown incident succeeded")
	}
	select {
	case e := <-ch:
		t.Fatalf("subscriber got %+v after a failed append", e)
	case e := <-all:
		t.Fatalf("all-subscriber got %+v after a failed append", e)
	default:
	}
}

func TestLogAction_KeepsAppliedAt(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	if err := j.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "refresh_links", TriggeredBy: "owner", Status: db.ActionFailed, AppliedAt: &at,
	}, false); err != nil {
		t.Fatal(err)
	}
	if e := lastEvent(t, j, inc.ID); !e.At.Equal(at) {
		t.Errorf("event at = %v, want %v", e.At, at)
	}
	actions, err := d.ListActions(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].AppliedAt == nil || !actions[0].AppliedAt.Equal(at) {
		t.Fatalf("actions = %+v, want one row applied at %v", actions, at)
	}
	if actions[0].Status != db.ActionFailed {
		t.Errorf("status = %q, want failed", actions[0].Status)
	}
}

func TestRunExit_CarriesExitAndRounds(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")
	ch, unsubscribe := j.Subscribe(inc.ID)
	defer unsubscribe()

	finding := map[string]any{"root_cause": "manual"}
	if err := j.RunExit(ctx, inc.ID, finding, 4, "budget_tripped", errors.New("limit")); err != nil {
		t.Fatal(err)
	}
	e := lastEvent(t, j, inc.ID)
	var p exitPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if e.Kind != string(journal.KindRunFinished) || p.Rounds != 4 || p.Exit != "budget_tripped" || p.Error != "limit" {
		t.Fatalf("event %s %+v, want run_finished rounds=4 exit=budget_tripped error=limit", e.Kind, p)
	}
	got, err := d.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := got.Finding.(map[string]any); m["root_cause"] != "manual" {
		t.Errorf("finding = %v, want the manual finding", got.Finding)
	}
	select {
	case n := <-ch:
		if n.Kind != string(journal.KindRunFinished) {
			t.Errorf("notified kind %q", n.Kind)
		}
	case <-time.After(subscribeWaitTimeout):
		t.Fatal("no notify after the run exit")
	}
}

func TestRunExit_TypedNilFindingLeavesFinding(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")
	if err := d.SetIncidentFinding(ctx, inc.ID, map[string]any{"root_cause": "kept"}, nil); err != nil {
		t.Fatal(err)
	}
	var none *exitPayload
	if err := j.RunExit(ctx, inc.ID, none, 1, "superseded", nil); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := got.Finding.(map[string]any); m["root_cause"] != "kept" {
		t.Fatalf("finding = %v, want kept", got.Finding)
	}
}

func TestRunFinished_StillAppends(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")
	if err := j.RunFinished(ctx, inc.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	e := lastEvent(t, j, inc.ID)
	var p exitPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if e.Kind != string(journal.KindRunFinished) || p.Rounds != 2 || p.Exit != "" {
		t.Fatalf("event %s %+v, want run_finished rounds=2 with no exit", e.Kind, p)
	}
}
