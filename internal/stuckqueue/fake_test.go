package stuckqueue_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
)

type write struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

type fakeArr struct {
	mu           sync.Mutex
	srv          *httptest.Server
	records      []map[string]any
	candidates   map[string]string
	writes       []write
	failQueue    bool
	failCommand  map[string]bool
	nextID       int
	gate         chan struct{}
	commands     []map[string]any
	commandsErr  int
	episodes     []map[string]any
	reqs         int
	deleteCode   int
	deleteLands  bool
	history      []map[string]any
	movies       []int
	failEpisodes bool
	arrived      int
}

func newFakeArr(t *testing.T) *fakeArr {
	t.Helper()
	f := &fakeArr{candidates: map[string]string{}, failCommand: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeArr) client() *client.ArrClient { return client.NewArr(f.srv.URL, "key") }

func (f *fakeArr) loadFixture(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Records []map[string]any `json:"records"`
	}
	if err = json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.records = page.Records
	f.mu.Unlock()
}

func (f *fakeArr) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	gate := f.gate
	f.arrived++
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs++
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
		f.serveQueue(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/manualimport":
		c, ok := f.candidates[r.URL.Query().Get("downloadId")]
		if !ok {
			c = "[]"
		}
		_, _ = io.WriteString(w, c)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/command":
		if f.commandsErr != 0 {
			w.WriteHeader(f.commandsErr)
			return
		}
		_ = json.NewEncoder(w).Encode(f.commands)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/episode":
		if f.failEpisodes {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(f.episodes)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/queue/"):
		f.serveDelete(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/history/"):
		_ = json.NewEncoder(w).Encode(f.history)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/series/"):
		_, _ = io.WriteString(w, `{"id":1}`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie":
		f.serveMovies(w)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
		body := f.record(r)
		if name, _ := body["name"].(string); f.failCommand[name] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.nextID++
		_ = json.NewEncoder(w).Encode(map[string]int{"id": f.nextID})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeArr) serveDelete(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	if f.deleteCode == 0 || f.deleteLands {
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/queue/"))
		f.dropDownloadOf(id)
	}
	if f.deleteCode != 0 {
		w.WriteHeader(f.deleteCode)
	}
}

func (f *fakeArr) serveMovies(w http.ResponseWriter) {
	out := make([]map[string]any, 0, len(f.movies))
	for _, id := range f.movies {
		out = append(out, map[string]any{"id": id})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeArr) serveQueue(w http.ResponseWriter, r *http.Request) {
	if f.failQueue {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("pageSize"))
	lo := min(max(page-1, 0)*size, len(f.records))
	hi := min(lo+size, len(f.records))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"page": page, "totalRecords": len(f.records), "records": f.records[lo:hi],
	})
}

func (f *fakeArr) record(r *http.Request) map[string]any {
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	f.writes = append(f.writes, write{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body})
	return body
}

func (f *fakeArr) dropDownloadOf(id int) {
	dl := ""
	for _, r := range f.records {
		if int(r["id"].(float64)) == id {
			dl, _ = r["downloadId"].(string)
		}
	}
	kept := f.records[:0]
	for _, r := range f.records {
		if d, _ := r["downloadId"].(string); dl == "" || !strings.EqualFold(d, dl) {
			kept = append(kept, r)
		}
	}
	f.records = kept
}

func (f *fakeArr) set(recs ...map[string]any) {
	f.mu.Lock()
	f.records = recs
	f.mu.Unlock()
}

func (f *fakeArr) writesCopy() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]write(nil), f.writes...)
}

func (f *fakeArr) setCandidates(downloadID, raw string) {
	f.mu.Lock()
	f.candidates[downloadID] = raw
	f.mu.Unlock()
}

func rawRec(id int, dl, title string, ep int, hasFile bool, msgs ...string) map[string]any {
	return map[string]any{
		"id": float64(id), "downloadId": dl, "title": title, "status": "completed",
		"trackedDownloadState": "importPending", "seriesId": float64(1), "episodeId": float64(ep),
		"seasonNumber": float64(1), "episodeHasFile": hasFile,
		"episode":        map[string]any{"episodeNumber": float64(ep)},
		"statusMessages": []map[string]any{{"messages": msgs}},
	}
}

func rawMovie(id int, dl, title string, movieID int, hasFile bool, msgs ...string) map[string]any {
	return map[string]any{
		"id": float64(id), "downloadId": dl, "title": title, "status": "completed",
		"trackedDownloadState": "importPending", "movieId": float64(movieID),
		"movie":          map[string]any{"hasFile": hasFile},
		"statusMessages": []map[string]any{{"messages": msgs}},
	}
}

func (f *fakeArr) hold() chan struct{} {
	g := make(chan struct{})
	f.mu.Lock()
	f.gate = g
	f.mu.Unlock()
	return g
}

func (f *fakeArr) setEpisodes(eps ...map[string]any) {
	f.mu.Lock()
	f.episodes = eps
	f.mu.Unlock()
}

func (f *fakeArr) setCommands(status int, cmds ...map[string]any) {
	f.mu.Lock()
	f.commandsErr, f.commands = status, cmds
	f.mu.Unlock()
}

func rawEpisode(id, season, number int, hasFile bool) map[string]any {
	return map[string]any{
		"id": float64(id), "seriesId": float64(1), "seasonNumber": float64(season),
		"episodeNumber": float64(number), "hasFile": hasFile,
	}
}

func (f *fakeArr) failDelete(code int, lands bool) {
	f.mu.Lock()
	f.deleteCode, f.deleteLands = code, lands
	f.mu.Unlock()
}

func (f *fakeArr) setHistory(recs ...map[string]any) {
	f.mu.Lock()
	f.history = recs
	f.mu.Unlock()
}

func (f *fakeArr) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func (f *fakeArr) awaitArrived(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.arrived
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no request reached the fake (want %d)", n)
}

func (f *fakeArr) setFailEpisodes() {
	f.mu.Lock()
	f.failEpisodes = true
	f.mu.Unlock()
}
