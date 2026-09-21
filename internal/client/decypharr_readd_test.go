package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

// readdStub serves decypharr's torrent list plus the delete and qBittorrent
// add endpoints, recording what it was called with.
type readdStub struct {
	torrents []map[string]any

	mu        sync.Mutex
	deleted   []string
	addedURLs []string
	addFails  bool
}

func (s *readdStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/torrents", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"torrents": s.torrents})
	})
	mux.HandleFunc("/api/torrents/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.mu.Lock()
		s.deleted = append(s.deleted, r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	// decypharr's add lives on its qBittorrent-compatible API, not its native
	// /api — confirmed against its own server.go, which mounts qbit at /api/v2.
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.addedURLs = append(s.addedURLs, r.FormValue("urls"))
		fails := s.addFails
		s.mu.Unlock()
		if fails {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func newReaddClient(t *testing.T, stub *readdStub) *client.DecypharrClient {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	return client.NewDecypharr(srv.URL, "tok")
}

// TestPlanTorrentReadd_RefusesWithoutAMagnet is the guard that makes this
// escalation safe to offer at all. The delete removes the content from the
// debrid provider; without a magnet there is nothing to put it back with, so
// approving would be a one-way operation with no undo. decypharr omits the
// field for entries that did not arrive as a magnet, so this is a real state,
// not a defensive hypothetical.
func TestPlanTorrentReadd_RefusesWithoutAMagnet(t *testing.T) {
	t.Parallel()
	stub := &readdStub{torrents: []map[string]any{
		{"name": "Show.S01E01", "info_hash": "abc", "category": "sonarr"},
	}}
	c := newReaddClient(t, stub)

	_, err := c.PlanTorrentReadd(context.Background(), "Show.S01E01")
	if !errors.Is(err, client.ErrNoMagnet) {
		t.Fatalf("err = %v, want ErrNoMagnet: deleting this torrent would be unrecoverable", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.deleted) != 0 {
		t.Error("planning deleted something; it must be read-only")
	}
}

// TestPlanTorrentReadd_RefusesAmbiguousAndMissingNames keeps the resolve step
// from guessing, for the same reason findFuzzyTitle refuses short titles: what
// it resolves to gets deleted.
func TestPlanTorrentReadd_RefusesAmbiguousAndMissingNames(t *testing.T) {
	t.Parallel()
	stub := &readdStub{torrents: []map[string]any{
		{"name": "Dupe", "info_hash": "a", "category": "sonarr", "magnet": "magnet:?xt=a"},
		{"name": "Dupe", "info_hash": "b", "category": "sonarr", "magnet": "magnet:?xt=b"},
	}}
	c := newReaddClient(t, stub)
	ctx := context.Background()

	if _, err := c.PlanTorrentReadd(ctx, "Dupe"); err == nil {
		t.Error("an ambiguous name resolved to one torrent; it must refuse")
	}
	if _, err := c.PlanTorrentReadd(ctx, "NotThere"); err == nil {
		t.Error("a name matching nothing produced a plan")
	}
	if _, err := c.PlanTorrentReadd(ctx, "  "); err == nil {
		t.Error("an empty name produced a plan")
	}
}

// TestExecuteTorrentReadd_DeletesThenReadds pins the order: the delete has to
// land first or the provider just returns the same cached entry.
func TestExecuteTorrentReadd_DeletesThenReadds(t *testing.T) {
	t.Parallel()
	stub := &readdStub{torrents: []map[string]any{
		{
			"name": "Show.S01E01", "info_hash": "abc", "category": "sonarr",
			"magnet": "magnet:?xt=urn:btih:abc", "state": "error", "debrid": "realdebrid",
		},
	}}
	c := newReaddClient(t, stub)
	ctx := context.Background()

	plan, err := c.PlanTorrentReadd(ctx, "Show.S01E01")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Magnet != "magnet:?xt=urn:btih:abc" || plan.Category != "sonarr" {
		t.Fatalf("plan = %+v", plan)
	}

	result, err := c.ExecuteTorrentReadd(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deleted || !result.Readded {
		t.Errorf("result = %+v, want both deleted and re-added", result)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.deleted) != 1 || !strings.Contains(stub.deleted[0], "abc") {
		t.Errorf("deleted = %v, want the planned hash", stub.deleted)
	}
	if len(stub.addedURLs) != 1 || stub.addedURLs[0] != plan.Magnet {
		t.Errorf("added = %v, want the planned magnet", stub.addedURLs)
	}
}

// TestExecuteTorrentReadd_FailedReaddReportsTheMagnet covers the window the
// plan step exists to narrow: if the add fails after the delete landed, the
// content is gone, and the error has to carry the magnet so a human can put it
// back by hand rather than having to go find it.
func TestExecuteTorrentReadd_FailedReaddReportsTheMagnet(t *testing.T) {
	t.Parallel()
	stub := &readdStub{
		torrents: []map[string]any{
			{
				"name": "Show.S01E01", "info_hash": "abc", "category": "sonarr",
				"magnet": "magnet:?xt=urn:btih:abc",
			},
		},
		addFails: true,
	}
	c := newReaddClient(t, stub)
	ctx := context.Background()

	plan, err := c.PlanTorrentReadd(ctx, "Show.S01E01")
	if err != nil {
		t.Fatal(err)
	}

	result, err := c.ExecuteTorrentReadd(ctx, plan)
	if err == nil {
		t.Fatal("a failed re-add reported success")
	}
	if !result.Deleted || result.Readded {
		t.Errorf("result = %+v, want deleted but not re-added", result)
	}
	if !strings.Contains(err.Error(), plan.Magnet) {
		t.Errorf("error %q does not carry the magnet; the content is gone and a human "+
			"needs it to recover", err)
	}
}
