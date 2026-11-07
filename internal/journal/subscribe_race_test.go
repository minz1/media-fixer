package journal_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/minz1/mediafixer/internal/journal"
)

func churnSubscribers(j *journal.Journal, incidentID string, stop *atomic.Bool, wg *sync.WaitGroup) {
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				_, unsubscribe := j.Subscribe(incidentID)
				_, unsubscribeOther := j.Subscribe(incidentID)
				unsubscribe()
				unsubscribeOther()
			}
		})
		wg.Go(func() {
			for !stop.Load() {
				_, unsubscribe := j.SubscribeAll()
				_, unsubscribeOther := j.SubscribeAll()
				unsubscribe()
				unsubscribeOther()
			}
		})
	}
}

func TestA8_5_ConcurrentAppendAndUnsubscribeNeverSendsOnClosedChannel(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")

	var stop atomic.Bool
	var churn sync.WaitGroup
	churnSubscribers(j, inc.ID, &stop, &churn)

	_, keepIncident := j.Subscribe(inc.ID)
	defer keepIncident()
	_, keepAll := j.SubscribeAll()
	defer keepAll()

	var appenders sync.WaitGroup
	for range 4 {
		appenders.Go(func() {
			for i := range 150 {
				if err := j.RunFinished(ctx, inc.ID, i, nil); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	appenders.Wait()
	stop.Store(true)
	churn.Wait()
}

func TestA10_11_UnsubscribeAllWhileAppendsRun(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")

	var stop atomic.Bool
	var appenders sync.WaitGroup
	for range 4 {
		appenders.Go(func() {
			for i := 0; !stop.Load(); i++ {
				if err := j.RunFinished(ctx, inc.ID, i, nil); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 100 {
		first, unsubscribeFirst := j.SubscribeAll()
		var unsubscribes []func()
		for range 64 {
			_, unsubscribe := j.SubscribeAll()
			unsubscribes = append(unsubscribes, unsubscribe)
		}
		<-first
		for _, unsubscribe := range unsubscribes {
			unsubscribe()
		}
		unsubscribeFirst()
	}
	stop.Store(true)
	appenders.Wait()
}
