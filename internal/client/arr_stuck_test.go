package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

const sonarrCandidateJSON = `[{"path":"/data/downloads/sonarr/NOVA.S09E01/nova.mkv","series":{"id":188},
"seasonNumber":9,"episodes":[{"id":14607,"seasonNumber":9,"episodeNumber":1,"hasFile":false}],
"quality":{"quality":{"id":1,"name":"SDTV"},"revision":{"version":1}},"languages":[{"id":1,"name":"English"}],
"releaseGroup":"X","indexerFlags":0,"releaseType":"singleEpisode","downloadId":"D",
"rejections":[{"reason":"Unable to determine if file is a sample","type":"permanent"}]}]`

type captured struct {
	method, path string
	query        map[string][]string
	body         []byte
}

func captureServer(t *testing.T, reply string) (*client.ArrClient, *[]captured) {
	t.Helper()
	var mu sync.Mutex
	var calls []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, captured{r.Method, r.URL.Path, r.URL.Query(), b})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return client.NewArr(srv.URL, "key"), &calls
}

func TestManualImportCandidates_Query(t *testing.T) {
	t.Parallel()
	c, calls := captureServer(t, `[]`)
	if _, err := c.ManualImportCandidates(context.Background(), "D1"); err != nil {
		t.Fatal(err)
	}
	got := (*calls)[0]
	if got.method != http.MethodGet || got.path != "/api/v3/manualimport" {
		t.Fatalf("call = %s %s", got.method, got.path)
	}
	want := map[string][]string{"downloadId": {"D1"}, "filterExistingFiles": {"false"}}
	if len(got.query) != len(want) || got.query["downloadId"][0] != "D1" ||
		got.query["filterExistingFiles"][0] != "false" {
		t.Fatalf("query = %v, want %v", got.query, want)
	}
	if _, ok := got.query["seriesId"]; ok {
		t.Fatal("seriesId must never be sent")
	}
}

func TestManualImportCandidates_Parses(t *testing.T) {
	t.Parallel()
	c, _ := captureServer(t, sonarrCandidateJSON)
	cs, err := c.ManualImportCandidates(context.Background(), "D")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].SeriesID != 188 || len(cs[0].Episodes) != 1 || cs[0].Episodes[0].ID != 14607 ||
		cs[0].Episodes[0].HasFile || cs[0].Episodes[0].EpisodeNumber != 1 || cs[0].ReleaseType != "singleEpisode" {
		t.Fatalf("candidates = %+v", cs)
	}
	if !slices.Equal(cs[0].Rejections, []string{"Unable to determine if file is a sample"}) {
		t.Fatalf("rejections = %v", cs[0].Rejections)
	}

	rc, _ := captureServer(t, `[{"path":"/m.mkv","movie":{"id":5,"hasFile":true},"rejections":[]}]`)
	ms, err := rc.ManualImportCandidates(context.Background(), "D")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].MovieID != 5 || !ms[0].MovieHasFile {
		t.Fatalf("radarr candidates = %+v", ms)
	}
}

func TestManualImport_Body(t *testing.T) {
	t.Parallel()
	c, calls := captureServer(t, `{"id":77}`)
	cands, err := func() ([]client.ImportCandidate, error) {
		pc, _ := captureServer(t, sonarrCandidateJSON)
		return pc.ManualImportCandidates(context.Background(), "D")
	}()
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.ManualImport(context.Background(), cands)
	if err != nil || id != 77 {
		t.Fatalf("id = %d, err = %v", id, err)
	}
	got := (*calls)[0]
	if got.method != http.MethodPost || got.path != "/api/v3/command" {
		t.Fatalf("call = %s %s", got.method, got.path)
	}
	var body struct {
		Name       string           `json:"name"`
		ImportMode string           `json:"importMode"`
		Files      []map[string]any `json:"files"`
	}
	if err = json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "ManualImport" || body.ImportMode != "move" || len(body.Files) != 1 {
		t.Fatalf("body = %s", got.body)
	}
	f := body.Files[0]
	for _, k := range []string{
		"path", "seriesId", "seasonNumber", "episodeIds", "quality", "languages",
		"releaseGroup", "indexerFlags", "releaseType", "downloadId",
	} {
		if _, ok := f[k]; !ok {
			t.Errorf("file is missing %q: %s", k, got.body)
		}
	}
	if ids, _ := f["episodeIds"].([]any); len(ids) != 1 || ids[0] != float64(14607) {
		t.Errorf("episodeIds = %v", f["episodeIds"])
	}
	if _, ok := f["movieId"]; ok {
		t.Error("a Sonarr file must not carry movieId")
	}

	rc, rcalls := captureServer(t, `{"id":78}`)
	movie := []client.ImportCandidate{{Path: "/m.mkv", MovieID: 5}}
	if _, err = rc.ManualImport(context.Background(), movie); err != nil {
		t.Fatal(err)
	}
	var rbody struct {
		Files []map[string]any `json:"files"`
	}
	if err = json.Unmarshal((*rcalls)[0].body, &rbody); err != nil {
		t.Fatal(err)
	}
	rf := rbody.Files[0]
	if rf["movieId"] != float64(5) {
		t.Errorf("radarr file = %v", rf)
	}
	for _, k := range []string{"seriesId", "episodeIds"} {
		if _, ok := rf[k]; ok {
			t.Errorf("a Radarr file must not carry %q", k)
		}
	}
}
