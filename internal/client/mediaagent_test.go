package client_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"io"
	"log/slog"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/mediaagent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

const testAPIKey = "test-secret"

// stubOps implements mediaagent.Ops without touching the OS.
type stubOps struct {
	ddResult   *mediaagentapi.DDTestResult
	restartErr error
	diskResult *mediaagentapi.DiskResult
}

func (s *stubOps) DDTest(_ string) (*mediaagentapi.DDTestResult, error) {
	return s.ddResult, nil
}

func (s *stubOps) Restart(_ string) error {
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

func newTestPair(t *testing.T, ops mediaagent.Ops) *client.MediaAgentClient {
	t.Helper()
	h := mediaagent.NewHandler(ops, testAPIKey, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client.NewMediaAgent(srv.URL, testAPIKey)
}

func TestMediaAgent_DDTest_OK(t *testing.T) {
	ops := &stubOps{ddResult: &mediaagentapi.DDTestResult{
		BytesRead: 104857600,
		SpeedMBs:  45.2,
	}}
	c := newTestPair(t, ops)

	result, err := c.DDReadabilityTest(context.Background(), "/mnt/fuse/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesRead != 104857600 {
		t.Errorf("bytes_read: got %d want 104857600", result.BytesRead)
	}
	if result.Error != "" {
		t.Errorf("unexpected error field: %s", result.Error)
	}
}

func TestMediaAgent_DDTest_IOError(t *testing.T) {
	ops := &stubOps{ddResult: &mediaagentapi.DDTestResult{
		BytesRead: 4096,
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
	c := newTestPair(t, &stubOps{})
	if err := c.RestartService(context.Background(), "jellyfin"); err != nil {
		t.Fatal(err)
	}
}

func TestMediaAgent_Restart_Failure(t *testing.T) {
	ops := &stubOps{restartErr: context.DeadlineExceeded}
	c := newTestPair(t, ops)

	if err := c.RestartService(context.Background(), "jellyfin"); err == nil {
		t.Fatal("expected error")
	}
}

func TestMediaAgent_Disk(t *testing.T) {
	ops := &stubOps{diskResult: &mediaagentapi.DiskResult{
		Mounts: []mediaagentapi.DiskMount{
			{Path: "/mnt", TotalBytes: 10 << 30, AvailableBytes: 4 << 30, UsedBytes: 6 << 30},
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
	h := mediaagent.NewHandler(&stubOps{}, testAPIKey, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Client with wrong key.
	c := client.NewMediaAgent(srv.URL, "wrong-key")
	if err := c.RestartService(context.Background(), "jellyfin"); err == nil {
		t.Fatal("expected auth error with wrong API key")
	}
}
