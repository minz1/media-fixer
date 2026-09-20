package server_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/server"
)

// TestDashboardTemplates_HxSSEAttributesHaveTheirExtensionLoaded is the
// regression test for the live-update outage: htmx 4 implements
// hx-sse:connect / hx-sse:close / <hx-partial> in dist/ext/hx-sse.js, NOT in
// htmx core. The dashboard shipped for weeks with the attributes present and
// the extension absent, so every hx-sse attribute was inert, no browser ever
// opened the stream, and the whole of stream.go was unreachable in production
// — while every server-side SSE test kept passing, because the gap was
// entirely on the client.
//
// Asserted as an implication (uses hx-sse ⇒ loads the extension) rather than
// a flat "this script tag exists" so it keeps holding if the attributes move
// to another template or are dropped altogether.
func TestDashboardTemplates_HxSSEAttributesHaveTheirExtensionLoaded(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "tester",
		What: "cant_play", Title: "Stream Fixture",
	}
	if err := database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/media/",
		"/media/incidents/" + inc.ID,
		"/media/incidents/" + inc.ID + "/transcript",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, "hx-sse:") {
			continue
		}
		if !strings.Contains(body, "ext/hx-sse.js") {
			t.Errorf("%s uses hx-sse attributes but never loads the hx-sse extension: "+
				"every hx-sse attribute on the page is inert", path)
		}
	}
}

// TestServerStart_ShutdownDoesNotHangOnOpenSSEConnection pins the shutdown
// fix. http.Server.Shutdown does not cancel in-flight request contexts, and
// the SSE handlers block until their request context is done — so before
// Server.shutdown existed, one open dashboard tab made Shutdown block
// forever and systemd SIGKILLed the unit at TimeoutStopSec on every deploy.
//
// The assertion is deliberately well under shutdownGrace: passing on the
// timeout alone would mean the handlers rode out the full grace period rather
// than observing the shutdown signal.
func TestServerStart_ShutdownDoesNotHangOnOpenSSEConnection(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	srv := newDashboardTestServerOnAddr(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	base := "http://" + addr
	waitForServer(t, base+"/media/")

	// Open a real SSE connection and leave it open. The handler is now parked
	// in its select loop, which is exactly the state that used to wedge
	// Shutdown.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/media/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE stream status = %d, want 200", resp.StatusCode)
	}
	// Read the first event so we know the handler is past startSSE and
	// actually sitting in the select loop, not still setting up.
	buf := make([]byte, 1)
	if _, err = resp.Body.Read(buf); err != nil {
		t.Fatalf("read first SSE byte: %v", err)
	}

	cancel()

	select {
	case err = <-done:
		if err != nil {
			t.Errorf("Start returned %v, want nil after clean shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s with an SSE connection open: " +
			"shutdown is blocked on the streaming handler")
	}
}

// freePort reserves and immediately releases a port, so Start's own
// ListenAndServe can bind it. Racy in principle, fine in a test process.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// waitForServer blocks until the server answers, so the test never races
// ListenAndServe's bind.
func waitForServer(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // short-lived readiness poll
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", url)
}

// newDashboardTestServerOnAddr is newDashboardTestServer bound to a concrete
// address, so a test can drive Start/Shutdown against a real listener rather
// than only the in-memory Handler.
func newDashboardTestServerOnAddr(t *testing.T, addr string) *server.Server {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	discard := slog.New(slog.DiscardHandler)
	jrnl := journal.New(database)
	svc := incident.NewService(context.Background(), database, jrnl, nil, nil, nil, &stubNotifier{}, discard)
	srv, err := server.New(addr, "/media", database, jrnl, svc, discard)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}
