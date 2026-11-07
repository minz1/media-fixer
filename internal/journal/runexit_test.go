package journal_test

import (
	"context"
	"encoding/json"
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
