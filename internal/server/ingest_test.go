package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"io"
	"log/slog"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

// stubNotifier satisfies incident.Notifier without a real Discord connection.
type stubNotifier struct{ msgs []string }

func (s *stubNotifier) NotifyOwner(_ context.Context, msg string) error {
	s.msgs = append(s.msgs, msg)
	return nil
}

func (s *stubNotifier) NotifyUser(_ context.Context, _, msg string) error {
	s.msgs = append(s.msgs, msg)
	return nil
}

func newTestServer(t *testing.T) (*Server, *db.DB) {
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

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	notif := &stubNotifier{}
	svc := incident.NewService(database, nil, nil, nil, notif, discard)
	srv := New(":0", "/media", database, svc, discard)
	return srv, database
}

func TestSeerrWebhook_CreatesIncident(t *testing.T) {
	srv, database := newTestServer(t)

	payload := seerrPayload{
		NotificationType: "ISSUE_CREATED",
		Subject:          "Breaking Bad",
		Message:          "can't play episode",
		IssueType:        "VIDEO",
		ReportedBy:       "alice",
		MediaJellyfinID:  "abc123",
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/ingest/seerr", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.handleSeerrWebhook(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201", rr.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	incID, ok := resp["incident_id"]
	if !ok || incID == "" {
		t.Fatal("expected incident_id in response")
	}

	inc, err := database.GetIncident(context.Background(), incID)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Title != "Breaking Bad" {
		t.Errorf("title: %q", inc.Title)
	}
	if inc.What != "cant_play" {
		t.Errorf("what: %q", inc.What)
	}
	if inc.JellyfinItemID != "abc123" {
		t.Errorf("jellyfin_item_id: %q", inc.JellyfinItemID)
	}
}

func TestSeerrWebhook_IgnoresNonCreate(t *testing.T) {
	srv, database := newTestServer(t)

	for _, notifType := range []string{"ISSUE_RESOLVED", "ISSUE_COMMENT", "MEDIA_AVAILABLE"} {
		payload := seerrPayload{NotificationType: notifType, Subject: "Some Title"}
		body, _ := json.Marshal(payload)

		req := httptest.NewRequest(http.MethodPost, "/ingest/seerr", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()

		srv.handleSeerrWebhook(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Errorf("%s: got %d want 204", notifType, rr.Code)
		}
	}

	// No incidents should have been created.
	incidents, err := database.ListIncidents(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 0 {
		t.Errorf("got %d incidents want 0", len(incidents))
	}
}

func TestSeerrWebhook_Deduplicates(t *testing.T) {
	srv, _ := newTestServer(t)

	send := func() string {
		payload := seerrPayload{
			NotificationType: "ISSUE_CREATED",
			Subject:          "The Wire",
			IssueType:        "VIDEO",
			ReportedBy:       "bob",
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/ingest/seerr", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleSeerrWebhook(rr, req)

		var resp map[string]string
		json.NewDecoder(rr.Body).Decode(&resp)
		return resp["incident_id"]
	}

	id1 := send()
	id2 := send()

	if id1 == "" || id2 == "" {
		t.Fatal("expected non-empty incident IDs")
	}
	if id1 != id2 {
		t.Errorf("duplicate reports should collapse: got %q and %q", id1, id2)
	}
}

func TestSeerrWebhook_BadJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/ingest/seerr", bytes.NewReader([]byte("not json{")))
	rr := httptest.NewRecorder()
	srv.handleSeerrWebhook(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("got %d want 400", rr.Code)
	}
}

func TestSeerrIssueTypeMapping(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"VIDEO", "cant_play"},
		{"AUDIO", "cant_play"},
		{"SUBTITLES", "cant_play"},
		{"OTHER", "other"},
		{"UNKNOWN", "other"},
	}
	for _, c := range cases {
		got := seerrIssueTypeToWhat(c.in)
		if got != c.want {
			t.Errorf("seerrIssueTypeToWhat(%q) = %q want %q", c.in, got, c.want)
		}
	}
}
