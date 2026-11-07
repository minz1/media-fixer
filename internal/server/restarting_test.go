package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/server"
)

func newServiceServer(t *testing.T) (*server.Server, *db.DB, *incident.Service) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	discard := slog.New(slog.DiscardHandler)
	jrnl := journal.New(database)
	svc := incident.NewService(context.Background(), database, jrnl, nil, nil, nil, &stubNotifier{}, discard)
	t.Cleanup(func() { stopService(t, svc) })
	srv, err := server.New(":0", "/media", database, jrnl, svc, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetSeerrWebhookSecret(testWebhookSecret)
	return srv, database, svc
}

func ownerPost(srv *server.Server, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestServer_OwnerPostWhileClosingShowsNotice(t *testing.T) {
	t.Parallel()
	srv, database, svc := newServiceServer(t)
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusManualTestNeeded, Source: "discord", What: "cant_play", Title: "Dark"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	stopService(t, svc)

	for _, verb := range []string{"rerun", "resolve", "unlock", "keep-searching"} {
		rec := ownerPost(srv, "/media/incidents/"+inc.ID+"/"+verb)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.HasSuffix(loc, "?done=restarting") {
			t.Errorf("%s while restarting = %d %q, want a redirect to the restarting notice", verb, rec.Code, loc)
		}
	}
	page := httptest.NewRecorder()
	srv.Handler().
		ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID+"?done=restarting", nil))
	if !strings.Contains(page.Body.String(), card.RestartingNotice) {
		t.Errorf("incident page does not show the restarting notice")
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusManualTestNeeded {
		t.Errorf("status = %s, want it unchanged", got.Status)
	}

	srv.BeginShutdownForTest()
	rec := ownerPost(srv, "/media/pause")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), card.RestartingNotice) {
		t.Errorf("POST after the HTTP shutdown began = %d %q, want 503 with the notice", rec.Code, rec.Body.String())
	}
	if paused, _ := database.IsAutonomousPaused(ctx); paused {
		t.Error("the pause POST reached the service after shutdown began")
	}
}

func seerrIssue(title string) map[string]any {
	return map[string]any{
		"notification_type": "ISSUE_CREATED", "subject": title, "message": "can't play", "issue_type": "VIDEO",
		"reported_by": "alice",
	}
}

func TestSeerrWebhook_ReportWhileClosingIsStored(t *testing.T) {
	t.Parallel()
	srv, database, svc := newServiceServer(t)
	stopService(t, svc)
	srv.BeginShutdownForTest()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postSeerr(t, ts, seerrIssue("Dark"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if _, err := database.FindOpenByTitle(context.Background(), "Dark"); err != nil {
		t.Errorf("report during shutdown not stored: %v", err)
	}
}

func TestSeerrWebhook_StoreFailureWhileClosingReturns503(t *testing.T) {
	t.Parallel()
	srv, database, _ := newServiceServer(t)
	srv.BeginShutdownForTest()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postSeerr(t, ts, seerrIssue("Dark"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so Seerr retries", resp.StatusCode)
	}
}
