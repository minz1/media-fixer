package server_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func playOp(t *testing.T, spec operation.Spec, events ...operation.Event) operation.Op {
	t.Helper()
	at := time.Now()
	events = append([]operation.Event{{Kind: operation.Created, At: at, Spec: &spec}}, events...)
	var op operation.Op
	for _, ev := range events {
		ev.At = at
		var err error
		if op, err = operation.Next(op, ev); err != nil {
			t.Fatal(err)
		}
	}
	return op
}

func saveOp(t *testing.T, database *db.DB, op operation.Op) operation.Op {
	t.Helper()
	saved, err := database.ApplyOperation(context.Background(), 0, func(operation.Op) (operation.Op, error) {
		return op, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func interruptedAfterStart(index int) []operation.Event {
	return []operation.Event{
		{Kind: operation.StepStarted, Index: index},
		{Kind: operation.Interrupted, Cause: operation.CauseRestart},
	}
}

func seedIncidentOp(t *testing.T, database *db.DB, spec operation.Spec) (string, operation.Op) {
	t.Helper()
	inc := &db.Incident{Status: db.StatusManualTestNeeded, Source: "discord", What: "cant_play", Title: "Dark"}
	lifecycletest.Incident(t, database, inc)
	spec.Subject = operation.Subject{Kind: operation.SubjectIncident, ID: inc.ID}
	op := playOp(t, spec, interruptedAfterStart(0)...)
	a, err := database.ApplyIncident(context.Background(), inc.ID, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		n.Status, n.Reason, n.Op = lifecycle.ManualTestNeeded, lifecycle.ReasonPlanPartial, &op
		var d db.Decision
		d.Next, d.OpWrite, d.OpNew = n, &op, true
		return d, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return inc.ID, *a.Snap.Op
}

func replaceOpSpec() operation.Spec {
	return operation.Spec{
		Kind: operation.KindReplace, Title: "Dark", Source: []byte(`{}`),
		Steps: []operation.Step{
			{Kind: operation.StepBlocklist, HistoryID: 3},
			{Kind: operation.StepSearch, App: "sonarr", Command: "SeasonSearch", MediaID: 5, Season: 1},
		},
	}
}

func TestDashboard_PartialOpShowsLineAndResume(t *testing.T) {
	t.Parallel()
	srv, database, _ := newServiceServer(t)
	id, op := seedIncidentOp(t, database, replaceOpSpec())

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/incidents/"+id, nil))

	body := rec.Body.String()
	for _, want := range []string{
		html.EscapeString(operation.Line(op)),
		html.EscapeString(operation.NextLine(op)),
		`class="info"`,
		"search",
		"/incidents/" + id + "/resume",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident page lacks %q", want)
		}
	}
}

func seedFailedRegrab(t *testing.T, e *stuckEnv) operation.Op {
	t.Helper()
	spec := operation.Spec{
		Subject: stuckqueue.OpSubject(stuckqueue.AppSonarr, "D"), Kind: operation.KindRegrab, Title: "One Piece",
		Source: []byte(`{}`),
		Steps: []operation.Step{
			{Kind: operation.StepQueueRemove, App: "sonarr", QueueID: 7, DownloadID: "D", MediaID: 1, Season: -1},
			{
				Kind: operation.StepSearch, App: "sonarr", MediaID: 1, Command: "EpisodeSearch",
				EpisodeIDs: []int{3}, Season: -1,
			},
		},
	}
	op := playOp(t, spec,
		operation.Event{Kind: operation.StepStarted, Index: 0},
		operation.Event{Kind: operation.StepFinished, Index: 0, Outcome: operation.ResultDone},
		operation.Event{Kind: operation.StepStarted, Index: 1},
		operation.Event{Kind: operation.Interrupted, Cause: operation.CauseRestart},
	)
	op = saveOp(t, e.db, op)
	seedActedRow(t, e, "D", string(stuckqueue.ActionRegrab), string(stuckqueue.ResultUnfinished), op.ID)
	return op
}

func resumeForm(app, id string, op operation.Op, version int64) url.Values {
	return url.Values{
		"app": {app}, "id": {id}, "op": {strconv.FormatInt(op.ID, 10)}, "v": {strconv.FormatInt(version, 10)},
	}
}

func TestDashboard_StuckFailedOpListed(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	op := seedFailedRegrab(t, env)

	rec := do(t, env.srv, http.MethodGet, "/media/maintenance", nil)

	body := rec.Body.String()
	mustContain(t, body,
		"One Piece",
		html.EscapeString(operation.Line(op)),
		"/media/maintenance/stuck-queue/resume",
		`name="app" value="s"`,
		`name="id" value="D"`,
		`name="op" value="`+strconv.FormatInt(op.ID, 10)+`"`,
		`name="v" value="`+strconv.FormatInt(op.Version, 10)+`"`,
		">Resume<",
	)
}

func TestDashboard_ResumeRouteNotShadowedByAction(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	op := seedFailedRegrab(t, env)

	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/stuck-queue/resume",
		resumeForm("s", "D", op, op.Version+1))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the stuck card (taken by {action}): %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), `id="stuck-queue"`, "this changed, look again")
}

func TestDashboard_ResumeStale(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	op := seedFailedRegrab(t, env)
	before := env.arr.requests()

	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/stuck-queue/resume",
		resumeForm("s", "D", op, op.Version+1))

	mustContain(t, rec.Body.String(), "this changed, look again", "One Piece")
	if env.arr.requests() != before {
		t.Error("a stale resume reached the service")
	}
	if got, err := env.db.Operation(context.Background(), op.ID); err != nil || got.Version != op.Version {
		t.Errorf("op after a stale resume = %+v, %v", got, err)
	}
}

func TestDashboard_StuckResumeRefusesOtherSubject(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	op := seedFailedRegrab(t, env)
	before := env.arr.requests()

	for _, form := range []url.Values{resumeForm("s", "OTHER", op, op.Version), resumeForm("r", "D", op, op.Version)} {
		rec := do(t, env.srv, http.MethodPost, "/media/maintenance/stuck-queue/resume", form)
		mustContain(t, rec.Body.String(), "this changed, look again")
	}
	if env.arr.requests() != before {
		t.Error("a mismatched subject probed the service")
	}
	if got, err := env.db.Operation(context.Background(), op.ID); err != nil || got.Version != op.Version {
		t.Errorf("op after a mismatched resume = %+v, %v", got, err)
	}
}
