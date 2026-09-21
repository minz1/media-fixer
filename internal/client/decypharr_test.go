package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

func TestDecypharr_ListTorrents(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/torrents" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing/wrong auth header")
		}
		_ = json.NewEncoder(w).Encode(client.TorrentListResponse{
			Torrents: []*client.TorrentEntry{
				{Name: "Breaking.Bad.S01", State: "seeding", InfoHash: "abc123"},
			},
			Total: 1,
		})
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "secret")
	torrents, err := c.ListTorrents(context.Background(), "Breaking", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 1 {
		t.Fatalf("got %d torrents want 1", len(torrents))
	}
	if torrents[0].InfoHash != "abc123" {
		t.Errorf("hash: %q", torrents[0].InfoHash)
	}
}

func TestDecypharr_RefreshLinks(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/repair/run" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["unrestrict_link"] != true {
			t.Errorf("expected unrestrict_link=true, got %v", body)
		}
		_ = json.NewEncoder(w).Encode(client.RepairRunResponse{RunID: "run-1"})
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	runID, err := c.RefreshLinks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-1" {
		t.Errorf("runID: %q", runID)
	}
}

func TestDecypharr_RepairStatus(t *testing.T) {
	t.Parallel()
	const sample = `{"running":true,"run_id":"run-9","progress":42}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/repair/status" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	raw, err := c.RepairStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != sample {
		t.Errorf("raw passthrough: got %q want %q", raw, sample)
	}
}

func TestDecypharr_RepairHealth(t *testing.T) {
	t.Parallel()
	const sample = `[{"name":"Breaking.Bad.S01","healthy":false}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/repair/health" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	raw, err := c.RepairHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != sample {
		t.Errorf("raw passthrough: got %q want %q", raw, sample)
	}
}

func TestDecypharr_MountCacheCleanup(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/mount/cache/cleanup" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	if err := c.MountCacheCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDecypharr_DeleteTorrent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method: %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/torrents/movies/deadbeef") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("removeFromDebrid") != "true" {
			t.Error("expected removeFromDebrid=true")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	if err := c.DeleteTorrent(context.Background(), "movies", "deadbeef", true); err != nil {
		t.Fatal(err)
	}
}

// TestDecypharr_ListTorrents_NotFoundIsAnError replaces an earlier test that
// asserted the opposite ("404 should be treated as no results"). That
// expectation was wrong: decypharr answers a search matching nothing with 200
// and an empty array, so a 404 means the /api/torrents route itself is gone —
// an upgrade moved or renamed it. Reporting that as "no torrents exist" told
// the agent the debrid content was missing, which points it at
// remove_and_search on files that are fine, and it also hid the condition
// from livecheck's classifyDecypharr, whose whole job is to flag a missing
// endpoint.
func TestDecypharr_ListTorrents_NotFoundIsAnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	if _, err := c.ListTorrents(context.Background(), "Nonexistent", ""); err == nil {
		t.Error("a 404 on /api/torrents must surface as an error: the route is missing, " +
			"which is not the same as the search matching nothing")
	}
}

// TestDecypharr_ListTorrents_EmptyResultIsEmpty is the case the old test
// meant to cover: a genuine no-match, which decypharr returns as 200.
func TestDecypharr_ListTorrents_EmptyResultIsEmpty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"torrents":[]}`))
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	torrents, err := c.ListTorrents(context.Background(), "Nonexistent", "")
	if err != nil {
		t.Fatalf("a 200 with an empty array is a valid no-results answer: %v", err)
	}
	if len(torrents) != 0 {
		t.Errorf("expected empty list, got %d", len(torrents))
	}
}

func TestDecypharr_ErrorResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := client.NewDecypharr(srv.URL, "")
	_, err := c.ListTorrents(context.Background(), "", "")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}
