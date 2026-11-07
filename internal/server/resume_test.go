package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/server"
)

func seedResumable(t *testing.T, database *db.DB) (string, operation.Op) {
	t.Helper()
	inc := &db.Incident{Status: db.StatusManualTestNeeded, Source: "discord", What: "cant_play", Title: "Dark"}
	lifecycletest.Incident(t, database, inc)
	spec := operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: inc.ID}, Kind: operation.KindReplace,
		Title: "Dark", Source: []byte(`{}`),
		Steps: []operation.Step{{
			Kind: operation.StepSearch, App: "sonarr", MediaID: 1, Command: "SeasonSearch", Season: 1,
		}},
	}
	at := time.Now()
	events := []operation.Event{
		{Kind: operation.Created, At: at, Spec: &spec},
		{Kind: operation.StepStarted, At: at, Index: 0},
		{Kind: operation.Interrupted, At: at, Cause: operation.CauseRestart},
	}
	var op operation.Op
	for _, ev := range events {
		var err error
		if op, err = operation.Next(op, ev); err != nil {
			t.Fatal(err)
		}
	}
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

func resumePost(srv *server.Server, id string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/media/incidents/"+id+"/resume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestResume_PageShowsTheButtonWithVersions(t *testing.T) {
	t.Parallel()
	srv, database, _ := newServiceServer(t)
	id, op := seedResumable(t, database)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/incidents/"+id, nil))

	body := rec.Body.String()
	for _, want := range []string{
		"/incidents/" + id + "/resume", fmt.Sprintf(`"op":"%d"`, op.ID), fmt.Sprintf(`"opv":"%d"`, op.Version),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident page lacks %q", want)
		}
	}
	if !strings.Contains(body, operation.NextLine(op)) {
		t.Errorf("incident page lacks %q beside the button", operation.NextLine(op))
	}
}

func TestResume_RefusesDuringShutdown(t *testing.T) {
	t.Parallel()
	srv, database, svc := newServiceServer(t)
	id, op := seedResumable(t, database)
	stopService(t, svc)

	rec := resumePost(
		srv,
		id,
		url.Values{"v": {"v1"}, "op": {strconv.FormatInt(op.ID, 10)}, "opv": {strconv.FormatInt(op.Version, 10)}},
	)

	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasSuffix(loc, "?done=restarting") {
		t.Errorf("resume while restarting = %d %q, want a redirect to the restarting notice", rec.Code, loc)
	}
	page := httptest.NewRecorder()
	srv.Handler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/media/incidents/"+id+"?done=restarting", nil))
	if !strings.Contains(page.Body.String(), card.RestartingNotice) {
		t.Error("incident page does not show the restarting notice")
	}
}

func TestResume_StaleOpVersionRedirectsToTheStaleNotice(t *testing.T) {
	t.Parallel()
	srv, database, svc := newServiceServer(t)
	id, op := seedResumable(t, database)
	v := svc.CurrentVersion(context.Background(), id)

	rec := resumePost(srv, id, url.Values{"v": {v.Token()}, "op": {strconv.FormatInt(op.ID, 10)}, "opv": {"999"}})

	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasSuffix(loc, "?done=stale") {
		t.Errorf("stale resume = %d %q, want a redirect to the stale notice", rec.Code, loc)
	}
}
