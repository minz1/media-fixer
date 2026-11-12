package journal_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

func TestPublish_NotifiesIncidentAndAllSubscribers(t *testing.T) {
	t.Parallel()
	j, _ := openTestJournal(t)
	ch, unsubscribe := j.Subscribe("inc-1")
	defer unsubscribe()
	all, unsubscribeAll := j.SubscribeAll()
	defer unsubscribeAll()
	e := &db.Event{IncidentID: "inc-1", Kind: string(journal.KindStatusChanged)}
	j.Publish(e)
	if got := <-ch; got != e {
		t.Fatalf("incident subscriber got %+v", got)
	}
	if got := <-all; got != e {
		t.Fatalf("all-subscriber got %+v", got)
	}
}

func TestF12_FullSubscriberIsMarkedDropped(t *testing.T) {
	t.Parallel()
	j, _ := openTestJournal(t)
	all, unsubscribeAll := j.SubscribeAll()
	defer unsubscribeAll()
	other, unsubscribeOther := j.SubscribeAll()
	defer unsubscribeOther()
	e := &db.Event{IncidentID: "inc-1", Kind: string(journal.KindStatusChanged)}
	for range cap(all) {
		j.Publish(e)
	}
	if j.Dropped(all) {
		t.Fatal("dropped before the buffer overflowed")
	}
	<-other
	j.Publish(e)
	if !j.Dropped(all) {
		t.Fatal("a skipped send was not recorded")
	}
	if j.Dropped(all) {
		t.Fatal("Dropped did not clear the flag")
	}
	if j.Dropped(other) {
		t.Fatal("a subscriber with room was marked dropped")
	}
}

func TestRunFinishedEvent_IsUnsentAndCarriesTheExit(t *testing.T) {
	t.Parallel()
	e := journal.RunFinishedEvent("inc-1", 3, "concluded", errors.New("boom"))
	if e.Seq != 0 || e.IncidentID != "inc-1" || e.Kind != string(journal.KindRunFinished) {
		t.Fatalf("event %+v", e)
	}
	var p exitPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.Rounds != 3 || p.Exit != "concluded" || p.Error != "boom" {
		t.Fatalf("payload %s %v", e.Payload, err)
	}
}
