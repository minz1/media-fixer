package stuckqueue_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func resumeD(s *stuckqueue.Scheduler, op operation.Op) (string, error) {
	return s.Resume(context.Background(), stuckqueue.AppSonarr, "D", op.ID, op.Version)
}

func TestStuckResume_DoneRecordsOk(t *testing.T) {
	t.Parallel()
	e, s, op := failSearch(t)
	e.clock.Add(time.Hour)
	notice, err := resumeD(s, op)
	if err != nil || notice != "Search sent for S01E01." {
		t.Fatalf("notice = %q, err = %v", notice, err)
	}
	row, ok := e.rowOf(t, "D")
	if !ok || row.State != string(stuckqueue.StateActed) || row.ActedResult != string(stuckqueue.ResultOK) ||
		row.ActedOpID != op.ID || !row.ActedAt.Equal(e.clock.Now()) {
		t.Fatalf("row = %+v, %v; want acted ok op %d at %s", row, ok, op.ID, e.clock.Now())
	}
}

func TestStuckResume_FailedAgainRestampsAndKeepsResume(t *testing.T) {
	t.Parallel()
	e, s, op := failSearch(t)
	e.sonarr.mu.Lock()
	e.sonarr.failCommand["EpisodeSearch"] = true
	e.sonarr.mu.Unlock()
	e.clock.Add(time.Hour)
	if _, err := resumeD(s, op); err == nil || errors.Is(err, stuckqueue.ErrRefused) {
		t.Fatalf("err = %v, want the search failure", err)
	}
	again := latestOp(t, e)
	if again.Version == op.Version || !operation.Resumable(again) {
		t.Fatalf("op = %+v, want a newer resumable version", again)
	}
	row, _ := e.rowOf(t, "D")
	if row.ActedResult != string(stuckqueue.ResultUnfinished) || row.ActedOpID != op.ID ||
		!row.ActedAt.Equal(e.clock.Now()) {
		t.Fatalf("row = %+v, want unfinished restamped to %s", row, e.clock.Now())
	}
	up, _ := render(t, s, stuckSub())
	a := resumeButton(t, up)
	if want := "o" + itoa(again.ID) + "." + itoa(again.Version); a.Args[2] != want {
		t.Fatalf("button op = %s, want %s", a.Args[2], want)
	}
}

func TestStuckResume_CrashBeforeResumedWriteCaughtUpAtStartup(t *testing.T) {
	t.Parallel()
	e, _, op := failSearch(t)
	s := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s.Wait)
	e.clock.Add(time.Hour)
	e.rawExec(t, `CREATE TRIGGER refuse_resumed BEFORE UPDATE ON stuck_downloads WHEN NEW.acted_result = 'ok'
		BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	if _, err := resumeD(s, op); err != nil {
		t.Fatal(err)
	}
	if got := latestOp(t, e); got.Status != operation.Done {
		t.Fatalf("setup: op = %+v", got)
	}
	if row, _ := e.rowOf(t, "D"); row.ActedResult != string(stuckqueue.ResultUnfinished) {
		t.Fatalf("setup: row = %+v, want the write refused", row)
	}
	e.rawExec(t, `DROP TRIGGER refuse_resumed`)
	s2 := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s2.Wait)
	adoptNow(t, s2)
	row, _ := e.rowOf(t, "D")
	if row.ActedResult != string(stuckqueue.ResultOK) || row.ActedOpID != op.ID {
		t.Fatalf("row = %+v, want acted ok", row)
	}
	if logLineWith(e, 0, adoptedMsg(0, 1)) == "" {
		t.Fatalf("logs = %s", e.logs.String())
	}
}

func deadAfterResume(t *testing.T) (*runEnv, *stuckqueue.Scheduler, operation.Op) {
	t.Helper()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusGatewayTimeout, false)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the removal timeout was not returned")
	}
	op := latestOp(t, e)
	if !operation.Resumable(op) {
		t.Fatalf("setup: op = %+v, want resumable", op)
	}
	e.sonarr.failDelete(http.StatusBadRequest, false)
	e.clock.Add(time.Hour)
	return e, s, op
}

func requireButtonsBack(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) {
	t.Helper()
	row, _ := e.rowOf(t, "D")
	if row.ActedResult == string(stuckqueue.ResultUnfinished) {
		t.Fatalf("row = %+v, want the dead op settled", row)
	}
	e.clock.Add(stuckqueue.Grace + time.Minute)
	e.runner.Run(context.Background(), liveSettings())
	up, ok := render(t, s, stuckSub())
	if !ok || !slices.Contains(buttonVerbs(t, up.Buttons), "regrab") {
		t.Fatalf("update = %+v, %v; want the owner buttons back", up, ok)
	}
}

func TestP6Review_ResumeOfDeadOpReturnsButtons(t *testing.T) {
	t.Parallel()
	e, s, op := deadAfterResume(t)
	if _, err := resumeD(s, op); err == nil || errors.Is(err, stuckqueue.ErrRefused) {
		t.Fatalf("err = %v, want the removal failure", err)
	}
	if got := latestOp(t, e); operation.Resumable(got) || got.Status != operation.FailedAtStep {
		t.Fatalf("setup: op = %+v, want failed for good", got)
	}
	requireButtonsBack(t, e, s)
}

func TestP6Review_CatchUpOfDeadOpReturnsButtons(t *testing.T) {
	t.Parallel()
	e, _, op := deadAfterResume(t)
	s := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s.Wait)
	e.rawExec(t, `CREATE TRIGGER refuse_resumed BEFORE UPDATE ON stuck_downloads
		BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	if _, err := resumeD(s, op); err == nil {
		t.Fatal("setup: the removal failure was not returned")
	}
	e.rawExec(t, `DROP TRIGGER refuse_resumed`)
	if row, _ := e.rowOf(t, "D"); row.ActedResult != string(stuckqueue.ResultUnfinished) {
		t.Fatalf("setup: row = %+v, want the write refused", row)
	}
	s2 := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s2.Wait)
	adoptNow(t, s2)
	requireButtonsBack(t, e, s2)
}

func TestStuckResume_ReleaseOnEveryPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name    string
		wantErr bool
		run     func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op) error
	}{
		{"row load error", true, func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			e.rawExec(t, `ALTER TABLE stuck_downloads RENAME TO stuck_gone`)
			_, err := resumeD(s, op)
			return err
		}},
		{"guard refuses", true, func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			e.rawExec(
				t,
				`UPDATE stuck_downloads SET state = 'waiting', offered = 'remove', acted_action = NULL, acted_at = NULL,
				acted_result = NULL, acted_op_id = NULL`,
			)
			_, err := resumeD(s, op)
			return err
		}},
		{"op id mismatch", true, func(_ *testing.T, _ *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			_, err := s.Resume(ctx, stuckqueue.AppSonarr, "D", op.ID+7, op.Version)
			return err
		}},
		{"subject mismatch", true, func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			seedRow(t, e, stuckqueue.Snapshot{
				Key: sonarrKey("E"), State: stuckqueue.StateActed, FirstSeen: e.clock.Now().Add(-time.Hour),
				Acted: &stuckqueue.Acted{
					Action: stuckqueue.ActionRegrab,
					At:     e.clock.Now(),
					Result: stuckqueue.ResultUnfinished,
					OpID:   op.ID,
				},
			})
			_, err := s.Resume(ctx, stuckqueue.AppSonarr, "E", op.ID, op.Version)
			return err
		}},
		{"stale version", true, func(_ *testing.T, _ *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			_, err := s.Resume(ctx, stuckqueue.AppSonarr, "D", op.ID, op.Version+1)
			return err
		}},
		{"not resumable", true, func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			if _, err := resumeD(s, op); err != nil {
				t.Fatal(err)
			}
			e.rawExec(t, `UPDATE stuck_downloads SET acted_result = 'unfinished'`)
			_, err := resumeD(s, latestOp(t, e))
			return err
		}},
		{"run failure", true, func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			e.sonarr.mu.Lock()
			e.sonarr.failCommand["EpisodeSearch"] = true
			e.sonarr.mu.Unlock()
			_, err := resumeD(s, op)
			return err
		}},
		{"resumed write failure", false, func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op) error {
			e.rawExec(t, `CREATE TRIGGER refuse_resumed BEFORE UPDATE ON stuck_downloads
				BEGIN SELECT RAISE(ABORT, 'refused'); END`)
			_, err := resumeD(s, op)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, s, op := failSearch(t)
			err := tc.run(t, e, s, op)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			requireRunNowFree(t, s)
		})
	}
	probeCases := map[string]func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler, op operation.Op){
		"probe read error": func(_ *testing.T, e *runEnv, _ *stuckqueue.Scheduler, _ operation.Op) { e.sonarr.srv.Close() },
		"probe conflict":   func(_ *testing.T, e *runEnv, _ *stuckqueue.Scheduler, _ operation.Op) { e.sonarr.failDelete(0, false) },
	}
	for name, mutate := range probeCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, s := regrabEnv(t)
			e.sonarr.failDelete(http.StatusGatewayTimeout, true)
			if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
				t.Fatal("setup: the removal timeout was not returned")
			}
			op := latestOp(t, e)
			mutate(t, e, s, op)
			if _, err := resumeD(s, op); err == nil {
				t.Fatal("the resume succeeded, want a refusal or error")
			}
			requireRunNowFree(t, s)
		})
	}
}

func TestStuckResume_StateAnswersDuringProbe(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusGatewayTimeout, true)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the removal timeout was not returned")
	}
	op := latestOp(t, e)
	e.sonarr.failDelete(0, false)
	e.sonarr.mu.Lock()
	arrived := e.sonarr.arrived
	e.sonarr.mu.Unlock()
	gate := e.sonarr.hold()
	done := make(chan error, 1)
	go func() {
		_, err := resumeD(s, op)
		done <- err
	}()
	e.sonarr.awaitArrived(t, arrived+1)
	answered := make(chan struct{})
	go func() {
		s.State()
		_, _ = s.CardRenderer()(context.Background(), stuckSub())
		close(answered)
	}()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		close(gate)
		<-done
		t.Fatal("State or the renderer blocked while the probe ran")
	}
	close(gate)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "left the queue") {
		t.Fatalf("err = %v", err)
	}
}

func TestL9_ResumeDoneNamesTargets(t *testing.T) {
	t.Parallel()
	_, s, op := failSearch(t)
	notice, err := resumeD(s, op)
	if err != nil || notice != "Search sent for S01E01." {
		t.Fatalf("notice = %q, err = %v", notice, err)
	}
}
