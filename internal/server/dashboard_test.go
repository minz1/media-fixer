package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/server"
)

// newDashboardTestServer builds a server backed by a real DB with no agent —
// enough to render pages without ever launching a background diagnostic run.
func newDashboardTestServer(t *testing.T) (*server.Server, *db.DB) {
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
	// Server and Service must share one Journal instance — see the identical
	// comment in escalation_test.go's newEscalationTestServer.
	jrnl := journal.New(database)
	svc := incident.NewService(context.Background(), database, jrnl, nil, nil, nil, &stubNotifier{}, discard)
	srv, err := server.New(":0", "/media", database, jrnl, svc, discard)
	if err != nil {
		t.Fatal(err)
	}
	return srv, database
}

// TestDashboardIndex_RendersWithHtmx4AndTailwind4 is the regression test for
// the htmx 2/Tailwind-v3-CDN → htmx 4/Tailwind 4 migration: the shared "head"
// define (see dictFunc) must actually resolve and render for the plain,
// unnamed top-level template, not just the "selftest"/"incident" ones every
// other test happens to exercise.
func TestDashboardIndex_RendersWithHtmx4AndTailwind4(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/media/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>Media Fixer</title>",
		"htmx.org@4.0.0/dist/htmx.min.js",
		"@tailwindcss/browser@4.3.3",
		"No incidents",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index page missing %q; body:\n%s", want, body)
		}
	}
}

// TestDashboardIncident_RendersWithHtmx4AndTailwind4 covers the "incident"
// define specifically, whose <title> is built from the incident ID via
// dictFunc + printf rather than a literal string.
func TestDashboardIncident_RendersWithHtmx4AndTailwind4(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "The Wire"}
	if err := database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>Incident " + inc.ID + " · Media Fixer</title>",
		"htmx.org@4.0.0/dist/htmx.min.js",
		"@tailwindcss/browser@4.3.3",
		"The Wire",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident page missing %q; body:\n%s", want, body)
		}
	}
}

// TestIncidentTemplate_ExecutesWithoutError directly captures
// ExecuteTemplate's error for every scenario the "incident" page (and its
// live-SSE re-render, which shares the exact same templates) can hit.
// Production handlers discard this error (`_ = s.tmpl.t.ExecuteTemplate`),
// so an HTTP-level 200-status check alone could miss a template bug that
// only manifests partway through rendering — Go's html/template writes as
// it goes, so a mid-render error still leaves a 200 already sent with
// whatever was written before the error looking like a complete page.
func TestIncidentTemplate_ExecutesWithoutError(t *testing.T) {
	t.Parallel()

	t.Run("no finding yet", func(t *testing.T) {
		t.Parallel()
		srv, database := newDashboardTestServer(t)
		inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T"}
		if err := database.CreateIncident(context.Background(), inc); err != nil {
			t.Fatal(err)
		}
		mustExecuteIncidentTemplate(t, srv, inc.ID, false)
	})

	t.Run("remove_and_search escalation recommended", func(t *testing.T) {
		t.Parallel()
		srv, database := newDashboardTestServer(t)
		inc := newManualTestNeededIncident(t, database)
		body := mustExecuteIncidentTemplate(t, srv, inc.ID, false)
		if !strings.Contains(body, "remove_and_search") || !strings.Contains(body, "Approve &amp; Run") {
			t.Errorf("expected the escalation card to render, got:\n%s", body)
		}
	})

	t.Run("pending outcome with keep-searching", func(t *testing.T) {
		t.Parallel()
		srv, database := newDashboardTestServer(t)
		ctx := context.Background()
		inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "Rick and Morty"}
		if err := database.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		po := &db.PendingOutcome{MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9, StartedAt: time.Now()}
		if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded); err != nil {
			t.Fatal(err)
		}
		body := mustExecuteIncidentTemplate(t, srv, inc.ID, false)
		if !strings.Contains(body, "Keep searching") {
			t.Errorf("expected the keep-searching card to render, got:\n%s", body)
		}
	})

	t.Run("full transcript page", func(t *testing.T) {
		t.Parallel()
		srv, database := newDashboardTestServer(t)
		inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T"}
		if err := database.CreateIncident(context.Background(), inc); err != nil {
			t.Fatal(err)
		}
		data, err := srv.BuildIncidentPageDataForTest(context.Background(), inc.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if execErr := srv.ExecuteTemplateForTest(&buf, "transcript_page", data); execErr != nil {
			t.Fatalf("template execution error: %v", execErr)
		}
	})
}

// mustExecuteIncidentTemplate builds the incident page's data and executes
// the "incident" template directly, failing the test on any execution error,
// and returns the rendered body for content assertions.
func mustExecuteIncidentTemplate(t *testing.T, srv *server.Server, incidentID string, full bool) string {
	t.Helper()
	data, err := srv.BuildIncidentPageDataForTest(context.Background(), incidentID, full)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if execErr := srv.ExecuteTemplateForTest(&buf, "incident", data); execErr != nil {
		t.Fatalf("template execution error: %v", execErr)
	}
	return buf.String()
}

// TestIncidentEvents_SettledIncidentClosesImmediately confirms an incident
// already in a terminal state (see incidentSettled) sends its render once,
// followed by a named "settled" event, and returns — never entering the
// blocking select loop, so this test has no timing dependency.
func TestIncidentEvents_SettledIncidentClosesImmediately(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := newManualTestNeededIncident(t, database)

	req := httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID+"/events", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "incident-card") {
		t.Errorf("expected an initial incident-card partial, got:\n%s", body)
	}
	if !strings.Contains(body, "event: settled") {
		t.Errorf("expected a settled close event, got:\n%s", body)
	}
}

// TestIncidentEvents_OpenIncidentStreamsThenClosesOnContextDone confirms a
// non-terminal incident's stream sends its initial render and then blocks
// (does not send "settled") until the request context is done, at which
// point it returns instead of leaking the goroutine indefinitely.
func TestIncidentEvents_OpenIncidentStreamsThenClosesOnContextDone(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := &db.Incident{
		Status: db.StatusInvestigating, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T",
	}
	if err := database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sseTestTimeout)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID+"/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req) // blocks until ctx times out, then returns

	body := rec.Body.String()
	if !strings.Contains(body, "incident-transcript") {
		t.Errorf("expected an initial incident-transcript partial, got:\n%s", body)
	}
	if strings.Contains(body, "event: settled") {
		t.Errorf("an investigating incident must not close as settled, got:\n%s", body)
	}
}

const sseTestTimeout = 150 * time.Millisecond

// TestDashboardEvents_RendersIncidentsListInitially confirms the dashboard-
// wide stream sends the current incidents list immediately on connect.
func TestDashboardEvents_RendersIncidentsListInitially(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Severance",
	}
	if err := database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sseTestTimeout)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/media/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "incidents-list") || !strings.Contains(body, "Severance") {
		t.Errorf("expected the incidents list partial naming the incident, got:\n%s", body)
	}
}

// TestIncidentTranscript_Renders confirms the full thought-process page
// renders for a real incident.
func TestIncidentTranscript_Renders(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Chernobyl",
	}
	if err := database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID+"/transcript", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Chernobyl", "Full thought process", "events?full=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("transcript page missing %q; body:\n%s", want, body)
		}
	}
}

// TestIncidentExport_DownloadsBundleWithEventsAndActions confirms the export
// endpoint returns a well-formed, complete JSON bundle: the incident, its
// actions, and its full event log — not a hand-picked subset.
func TestIncidentExport_DownloadsBundleWithEventsAndActions(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	// Use journal.LogAction, not the plain database.LogAction, to also
	// produce the action_applied event this test asserts on — production
	// code (Dispatcher.logAction) always goes through the journal; a bare
	// db.LogAction call is a standalone write with no event, by design.
	action := &db.ActionLog{
		IncidentID: inc.ID, Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied,
	}
	if err := journal.New(database).LogAction(ctx, action, false); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID+"/export", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}

	var bundle struct {
		Incident struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"incident"`
		Actions []struct {
			Action string `json:"action"`
		} `json:"actions"`
		Events []struct {
			Kind string `json:"kind"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("export body is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	if bundle.Incident.ID != inc.ID || bundle.Incident.Title != "T" {
		t.Errorf("incident section wrong: %+v", bundle.Incident)
	}
	if len(bundle.Actions) != 1 || bundle.Actions[0].Action != "clear_jellyfin_cache" {
		t.Errorf("actions section wrong: %+v", bundle.Actions)
	}
	if len(bundle.Events) == 0 {
		t.Error("expected at least one event (the action_applied event LogAction wrote), got none")
	}
}
