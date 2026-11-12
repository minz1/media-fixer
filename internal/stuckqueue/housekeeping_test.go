package stuckqueue_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func disableSweep(t *testing.T, e *runEnv) {
	t.Helper()
	set := stuckqueue.DefaultSettings()
	set.Enabled = false
	if err := stuckqueue.SaveSettings(context.Background(), e.db, set); err != nil {
		t.Fatal(err)
	}
}

func seedInflight(t *testing.T, e *runEnv, id string, cmd int, startedAgo time.Duration) {
	t.Helper()
	started := e.clock.Now().Add(-startedAgo)
	seedRows(t, e.db, stuckqueue.ToRowForTest(stuckqueue.Snapshot{
		Key: sonarrKey(id), State: stuckqueue.StateImportInflight, FirstSeen: started.Add(-time.Hour),
		NotifiedAt: started.Add(-30 * time.Minute),
		Import:     &stuckqueue.ImportCmd{CommandID: cmd, StartedAt: started, Files: stuckqueue.Files{N: 1, M: 2}},
	}))
}

func rowOf(t *testing.T, e *runEnv, id string) (stuckqueue.Snapshot, bool) {
	t.Helper()
	r, ok, err := e.db.GetStuck(context.Background(), "sonarr", id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return stuckqueue.Snapshot{}, false
	}
	return stuckqueue.FromRowForTest(r), true
}

func wantNoted(t *testing.T, e *runEnv, id string, res stuckqueue.ActResult) {
	t.Helper()
	got, ok := rowOf(t, e, id)
	if !ok || got.State != stuckqueue.StateNotified || got.Acted == nil || got.Acted.Result != res {
		t.Fatalf("row = %+v (stored %v), want notified with result %s", got, ok, res)
	}
}

func wantInflight(t *testing.T, e *runEnv, id string) {
	t.Helper()
	if got, ok := rowOf(t, e, id); !ok || got.State != stuckqueue.StateImportInflight {
		t.Fatalf("row = %+v (stored %v), want it still import_inflight", got, ok)
	}
}

func TestQ1_18_CommandCompletedMovesToActed(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	seedInflight(t, e, "D", 7, 10*time.Minute)
	e.sonarr.setCommandStatus(7, "completed")
	var refreshed []string
	s.Refresh = func(subject string) { refreshed = append(refreshed, subject) }
	e.tickAt(s, time.Minute)
	got, ok := rowOf(t, e, "D")
	if !ok || got.State != stuckqueue.StateActed || got.Acted == nil ||
		got.Acted.Action != stuckqueue.ActionImport || got.Acted.Result != stuckqueue.ResultOK {
		t.Fatalf("row = %+v (stored %v), want acted import ok", got, ok)
	}
	if len(refreshed) != 1 || !strings.Contains(refreshed[0], "d") {
		t.Fatalf("refreshed %v, want the download's card once", refreshed)
	}
}

func TestQ1_18_CommandFailedOffersImportAgain(t *testing.T) {
	t.Parallel()
	for i, status := range []string{"failed", "aborted", "cancelled", "orphaned"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			e, s := newSched(t)
			disableSweep(t, e)
			seedInflight(t, e, "D", 10+i, 10*time.Minute)
			e.sonarr.setCommandStatus(10+i, status)
			e.tickAt(s, time.Minute)
			wantNoted(t, e, "D", stuckqueue.ResultImportFailed)
		})
	}
}

func TestQ1_18_TimeoutOffersImportAgain(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	seedInflight(t, e, "D", 7, 61*time.Minute)
	e.sonarr.setCommandStatus(7, "started")
	e.tickAt(s, time.Minute)
	wantNoted(t, e, "D", stuckqueue.ResultImportTimeout)
}

func TestQ1_18_UnreadablePastTimeoutSaysUnconfirmed(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	seedInflight(t, e, "D", 7, 61*time.Minute)
	seedInflight(t, e, "E", 8, 10*time.Minute)
	e.sonarr.setCommandCode(7, http.StatusInternalServerError)
	e.sonarr.setCommandCode(8, http.StatusInternalServerError)
	e.tickAt(s, time.Minute)
	wantNoted(t, e, "D", stuckqueue.ResultImportUnconfirmed)
	wantInflight(t, e, "E")
}

func TestQ1_18_CommandGoneIsUnconfirmed(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	seedInflight(t, e, "D", 7, 10*time.Minute)
	e.tickAt(s, time.Minute)
	wantNoted(t, e, "D", stuckqueue.ResultImportUnconfirmed)
}

func TestQ1_18_UnknownStatusIsRunning(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	seedInflight(t, e, "D", 7, 10*time.Minute)
	seedInflight(t, e, "E", 8, 61*time.Minute)
	e.sonarr.setCommandStatus(7, "queued")
	e.sonarr.setCommandStatus(8, "somethingNew")
	e.tickAt(s, time.Minute)
	wantInflight(t, e, "D")
	wantNoted(t, e, "E", stuckqueue.ResultImportTimeout)
}

func TestImportPoll_CancelledReadEmitsNothing(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	disableSweep(t, e)
	seedInflight(t, e, "D", 7, 61*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := stuckqueue.NewScheduler(ctx, e.runner, e.db)
	t.Cleanup(s.Wait)
	gate := e.sonarr.hold()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Tick(e.clock.Now())
	}()
	e.sonarr.awaitArrived(t, 1)
	cancel()
	close(gate)
	<-done
	wantInflight(t, e, "D")
}

func TestImportPoll_RunsWhileSweepDisabled(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	seedInflight(t, e, "D", 7, 10*time.Minute)
	e.sonarr.setCommandStatus(7, "completed")
	s.Tick(e.clock.Now())
	s.Wait()
	if got, _ := rowOf(t, e, "D"); got.State != stuckqueue.StateActed {
		t.Fatalf("state %s, want acted", got.State)
	}
	if _, ok, _ := s.State(); ok {
		t.Fatal("a run happened while the sweep is disabled")
	}
}

func TestImportPoll_SkipsWhenSlotHeld(t *testing.T) {
	t.Parallel()
	for _, h := range []stuckqueue.Holder{stuckqueue.HolderRun, stuckqueue.HolderOwner} {
		t.Run(string(h), func(t *testing.T) {
			t.Parallel()
			e, s := newSched(t)
			disableSweep(t, e)
			seedInflight(t, e, "D", 7, 10*time.Minute)
			e.sonarr.setCommandStatus(7, "completed")
			release, err := stuckqueue.ClaimForTest(s, h, e.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			s.Tick(e.clock.Now())
			if n := e.sonarr.requests(); n != 0 {
				t.Fatalf("%d requests while the slot was held", n)
			}
			wantInflight(t, e, "D")
			if strings.Contains(e.logs.String(), "level=") {
				t.Fatalf("a held slot logged: %s", e.logs.String())
			}
			release()
			s.Tick(e.clock.Now())
			if got, _ := rowOf(t, e, "D"); got.State != stuckqueue.StateActed {
				t.Fatalf("state %s after release, want acted", got.State)
			}
		})
	}
}

func TestQ2_4_ActedRowExpiresWhileSweepDisabled(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	now := e.clock.Now()
	acted := func(id string, ago time.Duration) db.StuckRow {
		return stuckqueue.ToRowForTest(stuckqueue.Snapshot{
			Key: sonarrKey(id), State: stuckqueue.StateActed, FirstSeen: now.Add(-40 * 24 * time.Hour),
			Acted: &stuckqueue.Acted{Action: stuckqueue.ActionRemove, At: now.Add(-ago), Result: stuckqueue.ResultOK},
		})
	}
	seedRows(t, e.db, acted("OLD", 17*24*time.Hour), acted("NEW", 15*24*time.Hour))
	s.Tick(now)
	s.Wait()
	if _, ok := rowOf(t, e, "OLD"); ok {
		t.Fatal("the 17 day old outcome was kept")
	}
	if _, ok := rowOf(t, e, "NEW"); !ok {
		t.Fatal("the 15 day old outcome was pruned")
	}
}

func TestP6Followup_BadStoredRowIsPrunedWithOneError(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	disableSweep(t, e)
	now := e.clock.Now()
	seedRows(t, e.db, stuckqueue.ToRowForTest(stuckqueue.Snapshot{
		Key: sonarrKey("BAD"), State: stuckqueue.StateActed, FirstSeen: now.Add(-24 * time.Hour),
		Acted: &stuckqueue.Acted{
			Action: stuckqueue.ActionRemove, At: now.Add(-17 * 24 * time.Hour), Result: stuckqueue.ResultOK,
		},
	}))
	for range 3 {
		s.Tick(now)
		s.Wait()
	}
	if n := strings.Count(e.logs.String(), "level=ERROR"); n != 1 {
		t.Fatalf("%d ERROR lines over three passes, want 1", n)
	}
	if _, ok := rowOf(t, e, "BAD"); ok {
		t.Fatal("the bad row was kept")
	}
}
