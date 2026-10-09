package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/server"
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

func newTestServer(t *testing.T) (*server.Server, *db.DB) {
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
	notif := &stubNotifier{}
	jrnl := journal.New(database)
	svc := incident.NewService(context.Background(), database, jrnl, nil, nil, nil, notif, discard)
	srv, err := server.New(":0", "/media", database, jrnl, svc, discard)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetSeerrWebhookSecret(testWebhookSecret)
	return srv, database
}

const testWebhookSecret = "s3cret"

func postSeerr(t *testing.T, ts *httptest.Server, payload map[string]any) *http.Response {
	t.Helper()
	return postSeerrAuth(t, ts, payload, "Bearer "+testWebhookSecret)
}

func postSeerrAuth(t *testing.T, ts *httptest.Server, payload map[string]any, auth string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		ts.URL+"/ingest/seerr",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSeerrWebhook_CreatesIncident(t *testing.T) {
	t.Parallel()
	srv, database := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postSeerr(t, ts, map[string]any{
		"notification_type":     "ISSUE_CREATED",
		"subject":               "Breaking Bad",
		"message":               "can't play episode",
		"issue_type":            "VIDEO",
		"reported_by":           "alice",
		"media_jellyfinMediaId": "abc123",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d want 201", resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	incID, ok := result["incident_id"]
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
	t.Parallel()
	srv, database := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, notifType := range []string{"ISSUE_RESOLVED", "ISSUE_COMMENT", "MEDIA_AVAILABLE"} {
		resp := postSeerr(t, ts, map[string]any{
			"notification_type": notifType,
			"subject":           "Some Title",
		})
		resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s: got %d want 204", notifType, resp.StatusCode)
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
	t.Parallel()
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	payload := map[string]any{
		"notification_type": "ISSUE_CREATED",
		"subject":           "The Wire",
		"issue_type":        "VIDEO",
		"reported_by":       "bob",
	}

	sendAndGetID := func() string {
		resp := postSeerr(t, ts, payload)
		defer resp.Body.Close()
		var result map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&result)
		return result["incident_id"]
	}

	id1 := sendAndGetID()
	id2 := sendAndGetID()

	if id1 == "" || id2 == "" {
		t.Fatal("expected non-empty incident IDs")
	}
	if id1 != id2 {
		t.Errorf("duplicate reports should collapse: got %q and %q", id1, id2)
	}
}

func TestSeerrWebhook_BadJSON(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		ts.URL+"/ingest/seerr",
		bytes.NewReader([]byte("not json{")),
	)
	req.Header.Set("Authorization", "Bearer "+testWebhookSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d want 400", resp.StatusCode)
	}
}

func TestSeerrIssueTypeMapping(t *testing.T) {
	t.Parallel()
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
		got := server.SeerrIssueTypeToWhat(c.in)
		if got != c.want {
			t.Errorf("SeerrIssueTypeToWhat(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestSeerrWebhook_RejectsBadAuth(t *testing.T) {
	t.Parallel()
	srv, database := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	payload := map[string]any{"notification_type": "ISSUE_CREATED", "subject": "Andor"}
	for _, auth := range []string{"", "Bearer wrong", testWebhookSecret} {
		resp := postSeerrAuth(t, ts, payload, auth)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: got %d want 401", auth, resp.StatusCode)
		}
	}
	incidents, err := database.ListIncidents(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 0 {
		t.Errorf("got %d incidents want 0", len(incidents))
	}
}

func TestSeerrWebhook_UnconfiguredSecretFailsClosed(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	srv.SetSeerrWebhookSecret("")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postSeerrAuth(t, ts, map[string]any{"notification_type": "ISSUE_CREATED", "subject": "Andor"}, "Bearer ")
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("got %d want 503", resp.StatusCode)
	}
}

func TestSeerrWebhook_ReopenedLinksIssue(t *testing.T) {
	t.Parallel()
	srv, database := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postSeerr(t, ts, map[string]any{
		"notification_type": "ISSUE_REOPENED",
		"subject":           "Andor",
		"issue_id":          "42",
		"issue_type":        "VIDEO",
		"reported_by":       "alice",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d want 201", resp.StatusCode)
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	ids, err := database.ListSeerrIssueIDs(context.Background(), result["incident_id"])
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "42" {
		t.Errorf("seerr issue ids = %v, want [42]", ids)
	}
}

func TestSeerrWebhook_IgnoresNonNumericIssueID(t *testing.T) {
	t.Parallel()
	srv, database := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postSeerr(t, ts, map[string]any{
		"notification_type": "ISSUE_CREATED",
		"subject":           "Andor",
		"issue_id":          "{{issue_id}}",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status: got %d want 201", resp.StatusCode)
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	ids, err := database.ListSeerrIssueIDs(context.Background(), result["incident_id"])
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("seerr issue ids = %v, want none", ids)
	}
}
