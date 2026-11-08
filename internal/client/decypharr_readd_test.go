package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	torrents  []map[string]any
	total     int
	omitTotal bool

	mu        sync.Mutex
	deleted   []string
	addedURLs []string
	addFails  bool
}

func (s *readdStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/torrents", func(w http.ResponseWriter, _ *http.Request) {
		total := s.total
		if total == 0 {
			total = len(s.torrents)
		}
		body := map[string]any{"torrents": s.torrents, "total": total}
		if s.omitTotal {
			delete(body, "total")
		}
		_ = json.NewEncoder(w).Encode(body)
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

func TestPlanTorrentReadd_RefusesBeyondOnePage(t *testing.T) {
	t.Parallel()
	const (
		pageSize = 100
		total    = 150
	)
	torrents := make([]map[string]any, 0, pageSize)
	torrents = append(torrents, map[string]any{
		"name": "Show", "info_hash": "exact", "category": "sonarr", "magnet": "magnet:?xt=exact",
	})
	for i := len(torrents); i < pageSize; i++ {
		torrents = append(torrents, map[string]any{
			"name": fmt.Sprintf("Show.S01E%02d", i), "info_hash": fmt.Sprintf("h%d", i),
			"category": "sonarr", "magnet": fmt.Sprintf("magnet:?xt=h%d", i),
		})
	}
	stub := &readdStub{torrents: torrents, total: total}
	c := newReaddClient(t, stub)

	_, err := c.PlanTorrentReadd(context.Background(), "Show")
	if err == nil || !strings.Contains(err.Error(), "more than one page") {
		t.Fatalf("err = %v, want a refusal naming more than one page: a match on page two is never seen", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.deleted) != 0 {
		t.Error("planning deleted something; it must be read-only")
	}
}

func TestPlanTorrentReadd_PlansWhenTotalMissing(t *testing.T) {
	t.Parallel()
	stub := &readdStub{omitTotal: true, torrents: []map[string]any{
		{"name": "Show", "info_hash": "exact", "category": "sonarr", "magnet": "magnet:?xt=exact"},
	}}
	plan, err := newReaddClient(t, stub).PlanTorrentReadd(context.Background(), "Show")
	if err != nil || plan.Name != "Show" {
		t.Fatalf("plan = %+v, %v; want a plan for Show when decypharr sends no total", plan, err)
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
