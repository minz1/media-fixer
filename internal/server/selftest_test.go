package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

// TestSelftestIndex_Unconfigured verifies the page renders gracefully (not a
// 500) when no checker has been wired up via Server.SetChecker.
func TestSelftestIndex_Unconfigured(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/media/selftest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "not configured") {
		t.Errorf("expected unconfigured message, got: %s", body)
	}
}

// TestSelftestRun_Unconfigured verifies POSTing to run checks without a
// configured checker fails clearly instead of panicking on a nil dispatcher,
// and — the regression test for the htmx 2→4 migration, which made htmx swap
// every non-204/304 response instead of only 2xx — that the 503 body is a
// styled fragment, not the unstyled plain text [http.Error] used to produce
// (invisible under htmx 2's old swap-only-on-2xx default, but landing
// directly in #selftest-results verbatim under htmx 4's).
func TestSelftestRun_Unconfigured(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/media/selftest/run", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(body), `<p class="text-sm text-red-600">`) {
		t.Errorf("expected a styled error fragment, got: %s", body)
	}
	if !strings.Contains(string(body), "not configured") {
		t.Errorf("expected the not-configured message, got: %s", body)
	}
}

// TestSelftestRun_Configured verifies the full route wiring — form decoding,
// building a livecheck.Runner from the checker, and rendering the results
// fragment — against a stub backend that answers every request with an
// empty JSON object/array, without asserting on individual tool outcomes
// (those are livecheck's own unit tests' job).
func TestSelftestRun_Configured(t *testing.T) {
	t.Parallel()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte("[]"))
			return
		}
		w.Write([]byte("{}"))
	}))
	defer stub.Close()

	loki, err := client.NewLoki(stub.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServer(t)
	srv.SetChecker(&agent.Dispatcher{
		Decypharr: client.NewDecypharr(stub.URL, ""),
		Jellyfin:  client.NewJellyfin(stub.URL, ""),
		Sonarr:    client.NewArr(stub.URL, ""),
		Radarr:    client.NewArr(stub.URL, ""),
		Loki:      loki,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/media/selftest/run", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "write=false") {
		t.Errorf("expected rendered report table, got: %s", body)
	}
}

// TestSelftestRun_ConcurrentRequestsAreSingleFlighted is a regression test:
// two overlapping POST /selftest/run requests (a double-click, a page retry)
// previously both ran independently and could each call
// refresh_decypharr_links/decypharr_repair_sweep, racing each other into
// decypharr's own single-flight repair lock — confirmed live via decypharr's
// persisted run history showing two runs 5s apart from the same source, one
// of which decypharr correctly, but confusingly, rejected with 409. The
// second concurrent request here must now be rejected by mediafixer itself
// before ever reaching a backend.
func TestSelftestRun_ConcurrentRequestsAreSingleFlighted(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once

	mux := http.NewServeMux()
	// Sonarr's ListSeries is the first live call inside Run() (fixture
	// discovery); blocking it holds the whole run open long enough to prove
	// a second concurrent request gets rejected rather than also proceeding.
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		startOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte("[]"))
			return
		}
		w.Write([]byte("{}"))
	})
	stub := httptest.NewServer(mux)
	defer stub.Close()

	loki, err := client.NewLoki(stub.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServer(t)
	srv.SetChecker(&agent.Dispatcher{
		Decypharr: client.NewDecypharr(stub.URL, ""),
		Jellyfin:  client.NewJellyfin(stub.URL, ""),
		Sonarr:    client.NewArr(stub.URL, ""),
		Radarr:    client.NewArr(stub.URL, ""),
		Loki:      loki,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	firstStatus := make(chan int, 1)
	go func() {
		resp, postErr := ts.Client().Post(ts.URL+"/media/selftest/run", "application/x-www-form-urlencoded", nil)
		if postErr != nil {
			t.Error(postErr)
			firstStatus <- 0
			return
		}
		defer resp.Body.Close()
		firstStatus <- resp.StatusCode
	}()

	<-started // the first request is now blocked inside Run()

	resp2, err := ts.Client().Post(ts.URL+"/media/selftest/run", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second concurrent request status = %d, want 409", resp2.StatusCode)
	}

	close(release) // let the first request finish

	if got := <-firstStatus; got != http.StatusOK {
		t.Errorf("first request status = %d, want 200", got)
	}
}
