package stuckqueue_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

type write struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

type fakeArr struct {
	mu          sync.Mutex
	srv         *httptest.Server
	records     []map[string]any
	candidates  map[string]string
	writes      []write
	failQueue   bool
	failCommand map[string]bool
	nextID      int
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
	defer f.mu.Unlock()
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
