package agent_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

func TestFixLokiUnitSelector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "regex bare names get service suffix",
			input: `{unit=~"jellyfin|decypharr"}`,
			want:  `{unit=~"jellyfin\.service|decypharr\.service"}`,
		},
		{
			name:  "exact bare name gets service suffix",
			input: `{unit="jellyfin"}`,
			want:  `{unit="jellyfin.service"}`,
		},
		{
			name:  "regex already correct dot-escaped unchanged",
			input: `{unit=~"jellyfin\.service|decypharr\.service"}`,
			want:  `{unit=~"jellyfin\.service|decypharr\.service"}`,
		},
		{
			name:  "regex already correct unescaped dot unchanged",
			input: `{unit=~"jellyfin.service|decypharr.service"}`,
			want:  `{unit=~"jellyfin.service|decypharr.service"}`,
		},
		{
			name:  "exact already correct unchanged",
			input: `{unit="jellyfin.service"}`,
			want:  `{unit="jellyfin.service"}`,
		},
		{
			name:  "mixed: only bare name gets fixed",
			input: `{unit=~"jellyfin|decypharr.service"}`,
			want:  `{unit=~"jellyfin\.service|decypharr.service"}`,
		},
		{
			name:  "non-unit selector unchanged",
			input: `{job="systemd-journal"}`,
			want:  `{job="systemd-journal"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agent.FixLokiUnitSelector(tc.input)
			if got != tc.want {
				t.Errorf("fixLokiUnitSelector(%q)\n got  %q\n want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestWaitUntilReady is a regression test for restart_jellyfin/restart_decypharr
// reporting "restarted" as soon as systemd's restart command returns, even
// though the service inside hadn't finished starting yet (confirmed live:
// restart_jellyfin reported ok in 3.1s, but Jellyfin didn't accept
// connections until ~6s in, and the very next tool call in the same run hit
// the gap and got connection-refused).
func TestWaitUntilReady(t *testing.T) {
	t.Parallel()

	t.Run("succeeds immediately", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		probe := func(context.Context) error {
			calls.Add(1)
			return nil
		}
		err := agent.WaitUntilReadyForTest(context.Background(), time.Second, time.Millisecond, probe)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls.Load() != 1 {
			t.Errorf("probe called %d times, want 1", calls.Load())
		}
	})

	t.Run("succeeds after a few attempts within the timeout", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		probe := func(context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("not ready yet")
			}
			return nil
		}
		err := agent.WaitUntilReadyForTest(context.Background(), time.Second, time.Millisecond, probe)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls.Load() != 3 {
			t.Errorf("probe called %d times, want 3", calls.Load())
		}
	})

	t.Run("returns the last error if it never succeeds", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("still down")
		probe := func(context.Context) error { return wantErr }
		err := agent.WaitUntilReadyForTest(context.Background(), 30*time.Millisecond, 10*time.Millisecond, probe)
		if !errors.Is(err, wantErr) {
			t.Errorf("got %v, want %v", err, wantErr)
		}
	})

	t.Run("returns ctx error if canceled while waiting", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		probe := func(context.Context) error { return errors.New("still down") }
		err := agent.WaitUntilReadyForTest(ctx, time.Second, time.Millisecond, probe)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	})
}

// TestDispatchRestartJellyfin_WaitsForReady verifies restart_jellyfin only
// reports success once Jellyfin is actually responding, not as soon as
// systemctl's restart call returns.
func TestDispatchRestartJellyfin_WaitsForReady(t *testing.T) {
	t.Parallel()
	var pingCalls atomic.Int32
	jellyfin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Ping" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if pingCalls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer jellyfin.Close()

	mediaAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/restart/jellyfin" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mediaAgent.Close()

	disp := &agent.Dispatcher{
		Jellyfin:   client.NewJellyfin(jellyfin.URL, "key"),
		MediaAgent: client.NewMediaAgent(mediaAgent.URL, "key"),
	}
	result, err := disp.Call(context.Background(), "restart_jellyfin", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pingCalls.Load() != 3 {
		t.Errorf("Ping called %d times, want 3 (waited for readiness)", pingCalls.Load())
	}
	if m, ok := result.(map[string]string); !ok || m["status"] != "restarted" {
		t.Errorf("result = %+v, want status=restarted", result)
	}
}

// TestDispatchRestartDecypharr_WaitsForReady mirrors the Jellyfin test above
// for decypharr, whose readiness probe is RepairStatus rather than a
// dedicated ping endpoint.
func TestDispatchRestartDecypharr_WaitsForReady(t *testing.T) {
	t.Parallel()
	var statusCalls atomic.Int32
	decypharr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repair/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if statusCalls.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"enabled":true}`))
	}))
	defer decypharr.Close()

	mediaAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/restart/decypharr" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mediaAgent.Close()

	disp := &agent.Dispatcher{
		Decypharr:  client.NewDecypharr(decypharr.URL, "token"),
		MediaAgent: client.NewMediaAgent(mediaAgent.URL, "key"),
	}
	result, err := disp.Call(context.Background(), "restart_decypharr", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusCalls.Load() != 2 {
		t.Errorf("RepairStatus called %d times, want 2 (waited for readiness)", statusCalls.Load())
	}
	if m, ok := result.(map[string]string); !ok || m["status"] != "restarted" {
		t.Errorf("result = %+v, want status=restarted", result)
	}
}
