package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
// configured checker fails clearly instead of panicking on a nil dispatcher.
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

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
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
