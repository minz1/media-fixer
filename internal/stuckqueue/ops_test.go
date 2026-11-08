package stuckqueue_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func stuckSub() string { return card.StuckSubject("s", "D") }

func regrabEnv(t *testing.T) (*runEnv, *stuckqueue.Scheduler) {
	t.Helper()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	return e, s
}

func failSearch(t *testing.T) (*runEnv, *stuckqueue.Scheduler, operation.Op) {
	t.Helper()
	e, s := regrabEnv(t)
	e.sonarr.failCommand["EpisodeSearch"] = true
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the search failure was not returned")
	}
	e.sonarr.mu.Lock()
	e.sonarr.failCommand["EpisodeSearch"] = false
	e.sonarr.mu.Unlock()
	return e, s, latestOp(t, e)
}

func latestOp(t *testing.T, e *runEnv) operation.Op {
	t.Helper()
	op, err := e.db.LatestOperation(context.Background(), stuckqueue.OpSubject(stuckqueue.AppSonarr, "D"))
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func countWrites(f *fakeArr, method, suffix string) int {
	n := 0
	for _, w := range f.writesCopy() {
		if w.Method == method && strings.HasSuffix(w.Path, suffix) {
			n++
		}
	}
	return n
}

func resumeButton(t *testing.T, up card.Update) card.Action {
	t.Helper()
	if len(up.Buttons) != 1 || up.Buttons[0].Label != "Resume" || up.Buttons[0].Style != card.Secondary {
		t.Fatalf("buttons = %+v, want one secondary Resume", up.Buttons)
	}
	a, err := card.Decode(up.Buttons[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestQ2_8_RegrabSearchFailureSurvivesRestart(t *testing.T) {
	t.Parallel()
	e, _, op := failSearch(t)
	s2 := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s2.Wait)
	if err := s2.LoadOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	up, ok := render(t, s2, stuckSub())
	if !ok || up.Status != "⚠️ "+operation.Line(op) {
		t.Fatalf("update = %+v, %v; want the op line", up, ok)
	}
	a := resumeButton(t, up)
	want := []string{"s", "D", "o" + itoa(op.ID) + "." + itoa(op.Version)}
	if a.Kind != "sq" || a.Verb != "resume" || strings.Join(a.Args, "|") != strings.Join(want, "|") {
		t.Fatalf("action = %+v, want args %v", a, want)
	}
	if got := s2.FailedOperations(); len(got) != 1 || got[0].ID != op.ID {
		t.Fatalf("FailedOperations = %+v", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestQ5b_4_ResumeSendsTheSearchOnce(t *testing.T) {
	t.Parallel()
	e, s, op := failSearch(t)
	up, _ := render(t, s, stuckSub())
	a := resumeButton(t, up)
	res, err := s.CardHandler()(context.Background(), a)
	if err != nil || res.Line != "Search sent for NOVA." {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if n := countWrites(e.sonarr, http.MethodDelete, "/1"); n != 1 {
		t.Fatalf("removals = %d, want 1", n)
	}
	if n := countWrites(e.sonarr, http.MethodPost, "/command"); n != 2 {
		t.Fatalf("search commands = %d, want the failed one and one resumed", n)
	}
	if got := latestOp(t, e); got.Status != operation.Done || got.ID != op.ID {
		t.Fatalf("op = %+v", got)
	}
	before := e.sonarr.requests()
	_, err = s.Resume(context.Background(), stuckqueue.AppSonarr, "D", op.ID, op.Version+1)
	if !errors.Is(err, stuckqueue.ErrRefused) || e.sonarr.requests() != before {
		t.Fatalf("second resume err = %v, want a refusal without requests", err)
	}
	up, _ = render(t, s, stuckSub())
	if !strings.HasPrefix(up.Status, "✅ Re-grabbed") || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v", up)
	}
}

func TestQ5b_4_RemovalTimeoutThatLandedOffersResume(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusGatewayTimeout, true)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the removal timeout was not returned")
	}
	op := latestOp(t, e)
	up, _ := render(t, s, stuckSub())
	if !strings.Contains(up.Status, "The removal may have landed; search not sent.") {
		t.Fatalf("status = %q", up.Status)
	}
	resumeButton(t, up)
	e.sonarr.setHistory(map[string]any{
		"id": 9, "downloadId": "D", "eventType": "downloadFailed",
		"date": e.clock.Now().Add(time.Minute).Format("2006-01-02T15:04:05Z07:00"),
	})
	e.sonarr.failDelete(0, false)
	if _, err := s.Resume(context.Background(), stuckqueue.AppSonarr, "D", op.ID, op.Version); err != nil {
		t.Fatal(err)
	}
	got := latestOp(t, e)
	if got.Status != operation.Done || got.Results[0].State != operation.ResultDone || got.Results[0].Note == "" {
		t.Fatalf("op = %+v", got)
	}
	if n := countWrites(e.sonarr, http.MethodDelete, "/1"); n != 1 {
		t.Fatalf("removals = %d, want only the first", n)
	}
	if n := countWrites(e.sonarr, http.MethodPost, "/command"); n != 1 {
		t.Fatalf("searches = %d, want 1", n)
	}
}

func TestRegrab_RemovalStillQueuedResumesWithCurrentQueueID(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusGatewayTimeout, false)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the removal timeout was not returned")
	}
	op := latestOp(t, e)
	e.sonarr.failDelete(0, false)
	e.sonarr.set(rawRec(7, "D", "NOVA", 1, false, sample))
	if _, err := s.Resume(context.Background(), stuckqueue.AppSonarr, "D", op.ID, op.Version); err != nil {
		t.Fatal(err)
	}
	if n := countWrites(e.sonarr, http.MethodDelete, "/7"); n != 1 {
		t.Fatalf("writes = %+v, want a removal of queue id 7", e.sonarr.writesCopy())
	}
	if got := latestOp(t, e); got.Status != operation.Done {
		t.Fatalf("op = %+v", got)
	}
}

func TestRegrab_RemovalDefinitiveFailureIsNotResumable(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusBadRequest, false)
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab)
	if err == nil || errors.Is(err, stuckqueue.ErrRefused) {
		t.Fatalf("err = %v, want the removal failure", err)
	}
	op := latestOp(t, e)
	if op.Status != operation.FailedAtStep || operation.Resumable(op) || op.Results[0].State != operation.ResultFailed {
		t.Fatalf("op = %+v", op)
	}
	if got := s.FailedOperations(); len(got) != 0 {
		t.Fatalf("FailedOperations = %+v, want none", got)
	}
	if n := countWrites(e.sonarr, http.MethodPost, "/command"); n != 0 {
		t.Fatalf("searches = %d, want none", n)
	}
	if _, ok := render(t, s, stuckSub()); ok {
		t.Fatal("the card must not offer Resume")
	}
}

type flakyStore struct {
	d      *db.DB
	calls  atomic.Int32
	failAt int32
}

var errStore = errors.New("disk full")

func (f *flakyStore) ApplyOperation(
	ctx context.Context, id int64, decide func(operation.Op) (operation.Op, error),
) (operation.Op, error) {
	if f.calls.Add(1) == f.failAt {
		return operation.Op{}, errStore
	}
	return f.d.ApplyOperation(ctx, id, decide)
}

func TestRegrab_StoreFailureInterruptsOp(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	stuckqueue.SetOpStoreForTest(s, &flakyStore{d: e.db, failAt: 3})
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("want the store failure surfaced")
	}
	op := latestOp(t, e)
	if op.Status != operation.FailedAtStep || op.Cause != operation.CauseStore ||
		op.Results[0].State != operation.ResultUnknown {
		t.Fatalf("op = %+v, want failed_at_step by store with step 1 unknown", op)
	}
	up, _ := render(t, s, stuckSub())
	if !strings.Contains(up.Status, "database error") {
		t.Fatalf("status = %q", up.Status)
	}
	resumeButton(t, up)
}

func TestStuckResume_RefusesDuringShutdown(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	base, cancel := context.WithCancel(context.Background())
	cancel()
	s := stuckqueue.NewScheduler(base, e.runner, e.db)
	t.Cleanup(s.Wait)
	_, err := s.Resume(context.Background(), stuckqueue.AppSonarr, "D", 1, 0)
	if !errors.Is(err, card.ErrRestarting) {
		t.Fatalf("err = %v, want ErrRestarting", err)
	}
	if e.sonarr.requests() != 0 {
		t.Fatal("requests while closing")
	}
}

func TestStuckResume_StaleVersion(t *testing.T) {
	t.Parallel()
	e, s, op := failSearch(t)
	before := e.sonarr.requests()
	_, err := s.Resume(context.Background(), stuckqueue.AppSonarr, "D", op.ID, op.Version+1)
	if !errors.Is(err, stuckqueue.ErrRefused) || !strings.Contains(err.Error(), "this changed, look again") {
		t.Fatalf("err = %v", err)
	}
	if e.sonarr.requests() != before || latestOp(t, e).Version != op.Version {
		t.Fatal("a stale tap touched the service or the op")
	}
}

func TestStuckResume_RunActiveRefuses(t *testing.T) {
	t.Parallel()
	e, s, op := failSearch(t)
	gate := e.radarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	_, err := s.Resume(context.Background(), stuckqueue.AppSonarr, "D", op.ID, op.Version)
	close(gate)
	s.Wait()
	const want = "a stuck-queue run is active, try again in a moment"
	if !errors.Is(err, stuckqueue.ErrRefused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v", err)
	}
	if latestOp(t, e).Version != op.Version {
		t.Fatal("the op moved")
	}
}

func TestStuckResume_RefusesOtherSubject(t *testing.T) {
	t.Parallel()
	e, s, op := failSearch(t)
	before := e.sonarr.requests()
	for _, tc := range []struct {
		app stuckqueue.App
		id  string
	}{{stuckqueue.AppSonarr, "OTHER"}, {stuckqueue.AppRadarr, "D"}} {
		_, err := s.Resume(context.Background(), tc.app, tc.id, op.ID, op.Version)
		if !errors.Is(err, stuckqueue.ErrRefused) || !strings.Contains(err.Error(), "this changed, look again") {
			t.Fatalf("%s %s: err = %v", tc.app, tc.id, err)
		}
	}
	if e.sonarr.requests() != before || e.radarr.requests() != 0 || latestOp(t, e).Version != op.Version {
		t.Fatal("a mismatched subject probed or moved the op")
	}
}

func TestStuckResume_ConflictShowsNote(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusGatewayTimeout, true)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the removal timeout was not returned")
	}
	op := latestOp(t, e)
	e.sonarr.failDelete(0, false)
	_, err := s.Resume(context.Background(), stuckqueue.AppSonarr, "D", op.ID, op.Version)
	if !errors.Is(err, stuckqueue.ErrRefused) ||
		!strings.Contains(err.Error(), "the download left the queue without a blocklist record") {
		t.Fatalf("err = %v", err)
	}
	if got := latestOp(t, e); got.Version != op.Version || got.Status != operation.FailedAtStep {
		t.Fatalf("op = %+v", got)
	}
	if n := countWrites(e.sonarr, http.MethodPost, "/command"); n != 0 {
		t.Fatalf("searches = %d, want none", n)
	}
}
