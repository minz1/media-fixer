package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/server"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func wantRestartingTone(t *testing.T, where, body string) {
	t.Helper()
	if !strings.Contains(body, `<p class="notice tone-yellow">`+card.RestartingNotice+`</p>`) {
		t.Errorf("%s: no yellow restarting notice in\n%s", where, body)
	}
	if strings.Contains(body, `notice-ok">`+card.RestartingNotice) {
		t.Errorf("%s: restarting notice uses the success style", where)
	}
}

func getBody(srv *server.Server, path string) string {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Body.String()
}

func closedStuckQueue(database *db.DB) *stuckqueue.Scheduler {
	base, cancel := context.WithCancel(context.Background())
	cancel()
	return stuckqueue.NewScheduler(base, &stuckqueue.Runner{
		DB: database, Now: time.Now, Log: slog.New(slog.DiscardHandler),
	}, database)
}

func TestServer_RestartingNoticeIsYellowOnPages(t *testing.T) {
	t.Parallel()
	srv, database, svc := newServiceServer(t)
	inc := &db.Incident{Status: db.StatusManualTestNeeded, Source: "discord", What: "cant_play", Title: "Dark"}
	lifecycletest.Incident(t, database, inc)
	stopService(t, svc)

	wantRestartingTone(t, "incident page", getBody(srv, "/media/incidents/"+inc.ID+"?done=restarting"))
	wantRestartingTone(t, "dashboard", getBody(srv, "/media/?done=restarting"))
	wantRestartingTone(t, "preview", getBody(srv, "/media/incidents/"+inc.ID+"/escalation-preview"))

	req := httptest.NewRequest(http.MethodPost, "/media/pause", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Hx-Request", "true")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	wantRestartingTone(t, "pause", rec.Body.String())
}

func TestServer_RestartingNoticeIsYellowOnStuckQueue(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	sq := closedStuckQueue(database)
	t.Cleanup(sq.Wait)
	srv.SetStuckQueue(sq)
	for _, action := range []string{"import", "remove"} {
		rec := do(t, srv, http.MethodPost, "/media/maintenance/stuck-queue/"+action, url.Values{
			"app": {"sonarr"}, "download_id": {"D"}, "targets_fp": {"x"},
		})
		wantRestartingTone(t, "stuck-queue "+action, rec.Body.String())
	}
}

func TestServer_RestartingNoticeIsYellowOnMonitorImport(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	e.importSetup()
	sq := closedStuckQueue(e.db)
	t.Cleanup(sq.Wait)
	e.srv.SetStuckQueue(sq)
	rec := do(t, e.srv, http.MethodPost, importPath, url.Values{
		"download_id": {"d1"}, "targets_fp": {"x"}, "row": {"1|/dl/a.mkv"},
	})
	wantRestartingTone(t, "monitor import", rec.Body.String())
}

func TestServer_RefusedRequestScriptShowsRestartingInYellow(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)
	body := getBody(srv, "/media/")
	want := `text === "` + card.RestartingNotice + `" ? "tone-yellow" : ""`
	if !strings.Contains(body, want) {
		t.Errorf("dashboard script does not tone the restarting text yellow; want %q", want)
	}
}
