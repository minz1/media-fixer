package client_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/mediaagent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

const testAPIKey = "test-secret"

const (
	ddTestBytesRead100MB = 100 * 1024 * 1024
	ddTestBytesRead4KB   = 4 * 1024
	ddTestSpeedMBs       = 45.2
	diskTotalBytes       = 10 << 30
	diskAvailBytes       = 4 << 30
	diskUsedBytes        = 6 << 30
)

// stubOps implements mediaagent.Ops without touching the OS.
type stubOps struct {
	ddResult   *mediaagentapi.DDTestResult
	restartErr error
	diskResult *mediaagentapi.DiskResult
}

func (s *stubOps) DDTest(_ context.Context, _ string) (*mediaagentapi.DDTestResult, error) {
	return s.ddResult, nil
}

func (s *stubOps) Restart(_ context.Context, _ string) error {
	return s.restartErr
}

func (s *stubOps) DiskUsage() (*mediaagentapi.DiskResult, error) {
	if s.diskResult != nil {
		return s.diskResult, nil
	}
	return &mediaagentapi.DiskResult{}, nil
}

func (s *stubOps) ListDir(path string) (*mediaagentapi.ListDirResult, error) {
	return &mediaagentapi.ListDirResult{Path: path}, nil
}

func (s *stubOps) StuckReads() (*mediaagentapi.StuckReadsResult, error) {
	return &mediaagentapi.StuckReadsResult{Readers: []mediaagentapi.StuckReader{}}, nil
}

func newTestPair(t *testing.T, ops mediaagent.Ops) *client.MediaAgentClient {
	t.Helper()
	h := mediaagent.NewHandler(ops, testAPIKey, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client.NewMediaAgent(srv.URL, testAPIKey)
}

func TestMediaAgent_DDTest_OK(t *testing.T) {
	t.Parallel()
	ops := &stubOps{ddResult: &mediaagentapi.DDTestResult{
		BytesRead: ddTestBytesRead100MB,
		SpeedMBs:  ddTestSpeedMBs,
	}}
	c := newTestPair(t, ops)

	result, err := c.DDReadabilityTest(context.Background(), "/mnt/fuse/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesRead != ddTestBytesRead100MB {
		t.Errorf("bytes_read: got %d want %d", result.BytesRead, ddTestBytesRead100MB)
	}
	if result.Error != "" {
		t.Errorf("unexpected error field: %s", result.Error)
	}
}

func TestMediaAgent_DDTest_IOError(t *testing.T) {
	t.Parallel()
	ops := &stubOps{ddResult: &mediaagentapi.DDTestResult{
		BytesRead: ddTestBytesRead4KB,
		Error:     "read /mnt/fuse/movie.mkv: input/output error",
	}}
	c := newTestPair(t, ops)

	result, err := c.DDReadabilityTest(context.Background(), "/mnt/fuse/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if result.Error == "" {
		t.Error("expected EIO error in result")
	}
}

func TestMediaAgent_Restart_OK(t *testing.T) {
	t.Parallel()
	c := newTestPair(t, &stubOps{})
	if err := c.RestartService(context.Background(), "jellyfin"); err != nil {
		t.Fatal(err)
	}
}

func TestMediaAgent_Restart_Failure(t *testing.T) {
	t.Parallel()
	ops := &stubOps{restartErr: context.DeadlineExceeded}
	c := newTestPair(t, ops)

	if err := c.RestartService(context.Background(), "jellyfin"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMediaAgent_Disk(t *testing.T) {
	t.Parallel()
	ops := &stubOps{diskResult: &mediaagentapi.DiskResult{
		Mounts: []mediaagentapi.DiskMount{
			{Path: "/mnt", TotalBytes: diskTotalBytes, AvailableBytes: diskAvailBytes, UsedBytes: diskUsedBytes},
		},
	}}
	c := newTestPair(t, ops)

	result, err := c.DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Mounts) != 1 {
		t.Fatalf("mounts: got %d want 1", len(result.Mounts))
	}
	if result.Mounts[0].Path != "/mnt" {
		t.Errorf("path: %q", result.Mounts[0].Path)
	}
}

func TestMediaAgent_AuthRequired(t *testing.T) {
	t.Parallel()
	h := mediaagent.NewHandler(&stubOps{}, testAPIKey, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Client with wrong key.
	c := client.NewMediaAgent(srv.URL, "wrong-key")
	if err := c.RestartService(context.Background(), "jellyfin"); err == nil {
		t.Fatal("expected auth error with wrong API key")
	}
}

func TestClientStuckReads(t *testing.T) {
	t.Parallel()
	var gotAuth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(mediaagentapi.StuckReadsResult{
				Readers: []mediaagentapi.StuckReader{{PID: 7, Path: "/mnt/x"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(mediaagentapi.ErrorResponse{Error: "proc unreadable"})
	}))
	t.Cleanup(srv.Close)
	c := client.NewMediaAgent(srv.URL, testAPIKey)

	res, err := c.StuckReads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Readers) != 1 || res.Readers[0].PID != 7 || res.Readers[0].Path != "/mnt/x" {
		t.Errorf("readers = %+v", res.Readers)
	}
	if gotAuth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q", gotAuth)
	}

	status = http.StatusInternalServerError
	_, err = c.StuckReads(context.Background())
	if err == nil || !strings.Contains(err.Error(), "proc unreadable") {
		t.Errorf("err = %v, want it to contain the server's error text", err)
	}
}

func TestClientStuckReadsTimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := client.NewMediaAgent(srv.URL, testAPIKey)
	c.SetStuckReadsTimeoutForTest(50 * time.Millisecond)

	start := time.Now()
	if _, err := c.StuckReads(context.Background()); err == nil {
		t.Fatal("expected a timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("StuckReads took %s, want the 50 ms stuck-reads timeout", d)
	}
}

func TestClientStuckReadsTimeoutCoversBody(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"readers":[`))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := client.NewMediaAgent(srv.URL, testAPIKey)
	c.SetStuckReadsTimeoutForTest(50 * time.Millisecond)

	start := time.Now()
	if _, err := c.StuckReads(context.Background()); err == nil {
		t.Fatal("expected a timeout error while reading the body")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("StuckReads took %s, want the 50 ms stuck-reads timeout", d)
	}
}
