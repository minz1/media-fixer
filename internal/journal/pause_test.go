package journal_test

import (
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

func TestD4_12_PauseBroadcastReachesEverySubscriberAndIsNotStored(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	inc := mustCreateIncident(t, d, "T")

	one, unsubscribeOne := j.Subscribe(inc.ID)
	defer unsubscribeOne()
	all, unsubscribeAll := j.SubscribeAll()
	defer unsubscribeAll()

	j.AutonomousPauseChanged()

	for name, ch := range map[string]<-chan *db.Event{"incident": one, "all": all} {
		select {
		case e := <-ch:
			if e.Kind != string(journal.KindAutonomousPauseChanged) {
				t.Errorf("%s: kind = %q", name, e.Kind)
			}
		case <-time.After(subscribeWaitTimeout):
			t.Fatalf("%s: no broadcast", name)
		}
	}
	events, err := d.EventsByKind(t.Context(), string(journal.KindAutonomousPauseChanged), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("broadcast was stored: %+v", events)
	}
}
