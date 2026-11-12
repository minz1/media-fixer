package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
)

func postBazarr(t *testing.T, ts *httptest.Server, query string, auth func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/ingest/bazarr"+query,
		strings.NewReader(`{"message":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if auth != nil {
		auth(req)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

func TestC10_BazarrIngestAuth(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if code := postBazarr(t, ts, "?ep=1", basic("b", "hook")); code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status %d, want 503", code)
	}
	srv.SetBazarrWebhook("hook", &agent.BazarrWaits{})
	for _, tc := range []struct {
		query string
		auth  func(*http.Request)
	}{{"?ep=1", nil}, {"?ep=1", basic("b", "nope")}, {"?ep=1", bearer("hook")}, {"?ep=1&token=hook", nil}} {
		if code := postBazarr(t, ts, tc.query, tc.auth); code != http.StatusUnauthorized {
			t.Errorf("%s status %d, want 401", tc.query, code)
		}
	}
	for _, user := range []string{"bazarr", ""} {
		if code := postBazarr(t, ts, "?ep=1", basic(user, "hook")); code != http.StatusNoContent {
			t.Errorf("user %q status %d, want 204", user, code)
		}
	}
}

func TestC10_BazarrIngestWakesOnlyItsKey(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	waits := &agent.BazarrWaits{}
	srv.SetBazarrWebhook("hook", waits)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ep42, cancel42 := waits.Wait("ep:42")
	defer cancel42()
	ep43, cancel43 := waits.Wait("ep:43")
	defer cancel43()
	movie7, cancelM := waits.Wait("movie:7")
	defer cancelM()

	if code := postBazarr(t, ts, "?ep=42&series=3", basic("", "hook")); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if !closed(ep42) || closed(ep43) || closed(movie7) {
		t.Fatal("ep=42 must close only ep:42")
	}
	for _, q := range []string{"?movie=7", "?ep=&movie=7", "?ep=%7Bbazarr_episode_id%7D&movie=7"} {
		m, c := waits.Wait("movie:7")
		if code := postBazarr(t, ts, q, basic("", "hook")); code != http.StatusNoContent {
			t.Fatalf("%s status %d", q, code)
		}
		if !closed(m) {
			t.Errorf("%s did not wake movie:7", q)
		}
		c()
	}
	if code := postBazarr(t, ts, "?ep=99", basic("", "hook")); code != http.StatusNoContent {
		t.Fatalf("no-waiter status %d", code)
	}
	if closed(ep43) {
		t.Fatal("ep:43 woken by other pushes")
	}
}

func TestC10_NilWaitsSafe(t *testing.T) {
	t.Parallel()
	var w *agent.BazarrWaits
	ch, cancel := w.Wait("ep:1")
	if ch != nil {
		t.Fatal("nil Wait must return a nil channel")
	}
	cancel()
	w.Notify("ep:1")
}
