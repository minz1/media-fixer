package client_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/minz1/mediafixer/internal/client"
)

type scanRecorder struct {
	mu    sync.Mutex
	ended []time.Time
}

func (r *scanRecorder) add(_ context.Context, ended time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, ended)
}

func (r *scanRecorder) got() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.ended...)
}

func jellyfinSocket(t *testing.T, frames []string, auth chan<- string) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/socket" {
			http.NotFound(w, r)
			return
		}
		if auth != nil {
			select {
			case auth <- r.Header.Get("Authorization"):
			default:
			}
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		serveFrames(conn, frames)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serveFrames(conn *websocket.Conn, frames []string) {
	_, sub, err := conn.ReadMessage()
	if err != nil || !strings.Contains(string(sub), "ScheduledTasksInfoStart") {
		return
	}
	for _, f := range frames {
		if err = conn.WriteMessage(websocket.TextMessage, []byte(f)); err != nil {
			return
		}
	}
	for {
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
	}
}

const (
	scanEndedFrame = `{"MessageType":"ScheduledTaskEnded","Data":{"StartTimeUtc":"2026-11-08T17:00:00Z",` +
		`"EndTimeUtc":"2026-11-08T17:05:00Z","Status":"Completed","Name":"Scan Media Library",` +
		`"Key":"RefreshLibrary","Id":"a1"},"MessageId":"m1"}`
	otherTaskEndedFrame = `{"MessageType":"ScheduledTaskEnded","Data":{"StartTimeUtc":"2026-11-08T17:00:00Z",` +
		`"EndTimeUtc":"2026-11-08T17:06:00Z","Status":"Completed","Name":"Clean Cache","Key":"DeleteCacheFiles",` +
		`"Id":"b1"},"MessageId":"m2"}`
	scanRunningFrame = `{"MessageType":"ScheduledTasksInfo","Data":[{"Name":"Scan Media Library","State":"Running",` +
		`"CurrentProgressPercentage":37.5,"Id":"a1","Category":"Library","IsHidden":false,"Key":"RefreshLibrary"}],` +
		`"MessageId":"m3"}`
	scanIdleFrame = `{"MessageType":"ScheduledTasksInfo","Data":[{"Name":"Scan Media Library","State":"Idle",` +
		`"Id":"a1","LastExecutionResult":{"StartTimeUtc":"2026-11-08T18:00:00Z","EndTimeUtc":"2026-11-08T18:04:00Z",` +
		`"Status":"Completed","Name":"Scan Media Library","Key":"RefreshLibrary","Id":"a1"},"Key":"RefreshLibrary"}],` +
		`"MessageId":"m4"}`
	idleAgainFrame   = scanIdleFrame
	libraryChanged   = `{"MessageType":"LibraryChanged","Data":{"ItemsAdded":["0123456789abcdef0123456789abcdef"]},"MessageId":"m5"}`
	keepAliveRequest = `{"MessageType":"ForceKeepAlive","Data":60,"MessageId":"m6"}`
)

func TestE4_ParseMessagesFromFixture(t *testing.T) {
	t.Parallel()
	auth := make(chan string, 1)
	srv := jellyfinSocket(t, []string{
		keepAliveRequest, otherTaskEndedFrame, libraryChanged, scanEndedFrame, scanRunningFrame, scanIdleFrame,
		idleAgainFrame,
	}, auth)
	rec := &scanRecorder{}
	ws := client.NewJellyfinWS(srv.URL, "key-1")
	ws.OnScanFinished = rec.add
	ctx, cancel := context.WithCancel(t.Context())
	ws.Start(ctx)
	defer func() {
		cancel()
		ws.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.got()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	got := rec.got()
	want := []time.Time{
		time.Date(2026, 11, 8, 17, 5, 0, 0, time.UTC),
		time.Date(2026, 11, 8, 18, 4, 0, 0, time.UTC),
	}
	if len(got) != len(want) {
		t.Fatalf("scan-finished calls %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("scan-finished[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if a := <-auth; a != `MediaBrowser Token="key-1"` {
		t.Fatalf("Authorization %q", a)
	}
	if st := ws.State(); !st.Connected {
		t.Fatalf("state %+v, want connected", st)
	}
}

func TestE4_ReconnectBackoff(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var waits []time.Duration
	ws := client.NewJellyfinWS(srv.URL, "k")
	ctx, cancel := context.WithCancel(t.Context())
	ws.Sleep = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		waits = append(waits, d)
		n := len(waits)
		mu.Unlock()
		if n >= 4 {
			cancel()
			return false
		}
		return ctx.Err() == nil
	}
	ws.Start(ctx)
	ws.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 4 {
		t.Fatalf("waits %v, want 4", waits)
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Fatalf("backoff not increasing: %v", waits)
		}
	}
	if st := ws.State(); st.Connected || st.Err == "" {
		t.Fatalf("state %+v, want disconnected with an error", st)
	}
}

func TestE4_FlappingSocketKeepsBackingOff(t *testing.T) {
	t.Parallel()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	var mu sync.Mutex
	var waits []time.Duration
	ws := client.NewJellyfinWS(srv.URL, "k")
	ctx, cancel := context.WithCancel(t.Context())
	ws.Sleep = func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		waits = append(waits, d)
		n := len(waits)
		mu.Unlock()
		if n >= 4 {
			cancel()
			return false
		}
		return ctx.Err() == nil
	}
	ws.Start(ctx)
	ws.Wait()
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(waits); i++ {
		if waits[i] <= waits[i-1] {
			t.Fatalf("backoff reset by a session that dropped at once: %v", waits)
		}
	}
}

func TestE4_SilentSocketIsDropped(t *testing.T) {
	t.Parallel()
	up := websocket.Upgrader{}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	ws := client.NewJellyfinWS(srv.URL, "k")
	ws.ReadWait = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	dropped := make(chan struct{})
	ws.Sleep = func(context.Context, time.Duration) bool {
		close(dropped)
		cancel()
		return false
	}
	ws.Start(ctx)
	t.Cleanup(ws.Wait)
	t.Cleanup(cancel)
	select {
	case <-dropped:
	case <-time.After(5 * time.Second):
		t.Fatal("a socket that sends nothing was never dropped")
	}
}

func TestE4_ShutdownJoinsClient(t *testing.T) {
	t.Parallel()
	srv := jellyfinSocket(t, nil, nil)
	ws := client.NewJellyfinWS(srv.URL, "k")
	ctx, cancel := context.WithCancel(t.Context())
	ws.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !ws.State().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ws.State().Connected {
		t.Fatal("never connected")
	}
	cancel()
	done := make(chan struct{})
	go func() {
		ws.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestE4_JellyfinUnreachableNothingChanges(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	rec := &scanRecorder{}
	ws := client.NewJellyfinWS("http://"+addr, "k")
	ws.OnScanFinished = rec.add
	ctx, cancel := context.WithCancel(t.Context())
	tries := 0
	ws.Sleep = func(ctx context.Context, _ time.Duration) bool {
		tries++
		if tries >= 3 {
			cancel()
			return false
		}
		return ctx.Err() == nil
	}
	ws.Start(ctx)
	ws.Wait()
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("scan-finished called %v with Jellyfin unreachable", got)
	}
	if ws.State().Connected {
		t.Fatal("connected with Jellyfin unreachable")
	}

	off := client.NewJellyfinWS("", "")
	off.Start(t.Context())
	off.Wait()
	if off.State().Connected {
		t.Fatal("an unconfigured client reports connected")
	}
}
