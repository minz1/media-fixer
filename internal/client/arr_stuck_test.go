package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

func serveFile(t *testing.T, path string) *httptest.Server {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/queue" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func entryByID(t *testing.T, es []client.QueueEntry, id int) client.QueueEntry {
	t.Helper()
	for _, e := range es {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no entry %d", id)
	return client.QueueEntry{}
}

func TestQueueEntries_ParsesLiveSonarr(t *testing.T) {
	t.Parallel()
	srv := serveFile(t, "testdata/sonarr-queue-2026-10-29.json")
	es, err := client.NewArr(srv.URL, "key").QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 18 {
		t.Fatalf("entries = %d, want 18", len(es))
	}
	flash := entryByID(t, es, 1464435435)
	if flash.EpisodeID != 13781 || !flash.HasFile || !flash.HasFileKnown || flash.EpisodeNumber != 19 ||
		flash.SeasonNumber != 2 || flash.DownloadID == "" || flash.TrackedDownloadState != "importPending" {
		t.Errorf("flash = %+v", flash)
	}
	if op := entryByID(t, es, 1304969599); op.EpisodeID != 0 || op.HasFileKnown {
		t.Errorf("one piece = %+v", op)
	}
	lc := entryByID(t, es, 1910858291)
	if !slices.ContainsFunc(lc.Messages, func(m string) bool {
		return strings.HasPrefix(m, "This show has individual episode mappings on TheXEM")
	}) {
		t.Errorf("link click messages = %q", lc.Messages)
	}
}

func TestQueueEntries_ParsesLiveRadarr(t *testing.T) {
	t.Parallel()
	srv := serveFile(t, "testdata/radarr-queue-2026-10-29.json")
	es, err := client.NewArr(srv.URL, "key").QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].MovieID != 0 || es[0].HasFileKnown {
		t.Fatalf("entries = %+v", es)
	}
}

type pagedQueue struct {
	mu    sync.Mutex
	total int
	pages map[int][]int
	seen  []map[string]string
}

func (p *pagedQueue) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	p.mu.Lock()
	seen := map[string]string{}
	for k := range q {
		seen[k] = q.Get(k)
	}
	p.seen = append(p.seen, seen)
	ids := p.pages[page]
	p.mu.Unlock()
	recs := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		recs = append(recs, map[string]any{"id": id, "downloadId": "d" + strconv.Itoa(id), "status": "completed"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"page": page, "totalRecords": p.total, "records": recs})
}

func pagedServer(t *testing.T, p *pagedQueue) *client.ArrClient {
	t.Helper()
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return client.NewArr(srv.URL, "key")
}

func TestQueueEntries_Pages(t *testing.T) {
	t.Parallel()
	p := &pagedQueue{total: 3, pages: map[int][]int{1: {1, 2}, 2: {2, 3}}}
	es, err := pagedServer(t, p).QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("entries = %+v, want 3 unique", es)
	}
	if len(p.seen) != 2 || p.seen[0]["page"] != "1" || p.seen[1]["page"] != "2" {
		t.Fatalf("requests = %+v", p.seen)
	}
	for _, k := range []string{
		"includeUnknownSeriesItems", "includeUnknownMovieItems", "includeEpisode", "includeMovie",
	} {
		if p.seen[0][k] != "true" {
			t.Errorf("%s = %q, want true", k, p.seen[0][k])
		}
	}
	if p.seen[0]["pageSize"] != "250" {
		t.Errorf("pageSize = %q", p.seen[0]["pageSize"])
	}
}

func TestQueueEntries_ShortLastPage(t *testing.T) {
	t.Parallel()
	p := &pagedQueue{total: 5, pages: map[int][]int{1: {1, 2}}}
	es, err := pagedServer(t, p).QueueEntries(context.Background())
	if err != nil || len(es) != 2 {
		t.Fatalf("entries = %+v, err = %v", es, err)
	}
}

func TestQueueEntries_IgnoresPageParam(t *testing.T) {
	t.Parallel()
	p := &pagedQueue{total: 5, pages: map[int][]int{1: {1, 2}, 2: {1, 2}, 3: {1, 2}}}
	es, err := pagedServer(t, p).QueueEntries(context.Background())
	if err != nil || len(es) != 2 {
		t.Fatalf("entries = %+v, err = %v", es, err)
	}
	if len(p.seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(p.seen))
	}
}

func TestQueueEntries_ErrorHasNoBaseURL(t *testing.T) {
	t.Parallel()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	for name, base := range map[string]string{"500": failing.URL, "closed": closedURL} {
		_, err := client.NewArr(base, "key").QueueEntries(context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), "arr GET /api/v3/queue: ") ||
			strings.Contains(err.Error(), base) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestDo_CancelledStillIs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.NewArr(srv.URL, "key").QueueEntries(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
