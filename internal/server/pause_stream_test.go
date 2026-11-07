package server_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/server"
)

const (
	pausedLine = "Autonomous actions paused"
	liveStep   = "Working on step"
)

func openSSE(t *testing.T, ts *httptest.Server, path string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sseEventDeadline)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	events := make(chan string, 16)
	go readSSE(bufio.NewScanner(resp.Body), events)
	return events
}

func readSSE(scanner *bufio.Scanner, events chan<- string) {
	defer close(events)
	scanner.Buffer(nil, sseMaxLineBytes)
	var ev strings.Builder
	for scanner.Scan() {
		if scanner.Text() != "" {
			ev.WriteString(scanner.Text() + "\n")
			continue
		}
		if ev.Len() > 0 && !strings.HasPrefix(ev.String(), ":") {
			events <- ev.String()
		}
		ev.Reset()
	}
}

func nextSSE(t *testing.T, events <-chan string, wait time.Duration) (string, bool) {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev, true
	case <-time.After(wait):
		return "", false
	}
}

func renderAfterToggle(t *testing.T, srv *server.Server, events <-chan string, action string) string {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		rec := doWith(t, srv, http.MethodPost, "/media/"+action, url.Values{}, map[string]string{"HX-Request": "true"})
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /media/%s: %d", action, rec.Code)
		}
		if ev, ok := nextSSE(t, events, 100*time.Millisecond); ok {
			return ev
		}
	}
	t.Fatalf("no render after %s", action)
	return ""
}

func checkPauseRender(t *testing.T, ev string, paused, incident bool) {
	t.Helper()
	if !strings.Contains(ev, `id="pause-toggle"`) || strings.Contains(ev, pausedLine) != paused {
		t.Errorf("render (paused=%v) has the wrong pause toggle:\n%s", paused, ev)
	}
	if incident && strings.Contains(ev, liveStep) == paused {
		t.Errorf("render (paused=%v) has the wrong live step:\n%s", paused, ev)
	}
	if strings.Contains(ev, "event: settled") {
		t.Errorf("render closed the stream as settled:\n%s", ev)
	}
}

func TestD4_12_PauseAndResumeRerenderOpenStreams(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path string
		incident   bool
	}{
		{"incident", "/media/incidents/%s/events", true},
		{"transcript", "/media/incidents/%s/events?full=1", true},
		{"index", "/media/events", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, database := newDashboardTestServer(t)
			inc := &db.Incident{
				Status: db.StatusInvestigating, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T",
			}
			if err := database.CreateIncident(context.Background(), inc); err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			events := openSSE(t, ts, strings.Replace(tc.path, "%s", inc.ID, 1))
			first, ok := nextSSE(t, events, sseEventDeadline)
			if !ok {
				t.Fatal("no initial render")
			}
			checkPauseRender(t, first, false, tc.incident)
			checkPauseRender(t, renderAfterToggle(t, srv, events, "pause"), true, tc.incident)
			checkPauseRender(t, renderAfterToggle(t, srv, events, "resume"), false, tc.incident)
		})
	}
}
