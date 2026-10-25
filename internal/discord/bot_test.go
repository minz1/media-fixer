package discord_test

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/discord"
)

func testBot(t *testing.T, connect func() error) *discord.Bot {
	t.Helper()
	b, err := discord.NewForTest(slog.New(slog.DiscardHandler), connect, time.Millisecond, 4*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func failingConnect(calls *atomic.Int32) func() error {
	return func() error {
		calls.Add(1)
		return errors.New("lookup discord.com: server misbehaving")
	}
}

func waitForCalls(t *testing.T, calls *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("connect calls = %d, want at least %d", calls.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStart_RetriesUntilConnected(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	connected := make(chan struct{})
	b := testBot(t, func() error {
		if calls.Add(1) < 3 {
			return errors.New("lookup discord.com: server misbehaving")
		}
		close(connected)
		return nil
	})
	began := time.Now()
	b.Start(t.Context())
	if took := time.Since(began); took > 50*time.Millisecond {
		t.Fatalf("Start took %v, want it to return at once", took)
	}
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("never connected")
	}
	time.Sleep(10 * time.Millisecond)
	if n := calls.Load(); n != 3 {
		t.Errorf("connect calls = %d, want 3", n)
	}
}

func TestStart_CloseDuringBackoffStops(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	b := testBot(t, failingConnect(&calls))
	b.Start(t.Context())
	waitForCalls(t, &calls, 2)
	_ = b.Close()
	time.Sleep(5 * time.Millisecond)
	n := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != n {
		t.Errorf("connect calls grew from %d to %d after Close", n, got)
	}
}

func TestStart_ContextCancelStops(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	b := testBot(t, failingConnect(&calls))
	ctx, cancel := context.WithCancel(t.Context())
	b.Start(ctx)
	waitForCalls(t, &calls, 2)
	cancel()
	time.Sleep(5 * time.Millisecond)
	n := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != n {
		t.Errorf("connect calls grew from %d to %d after cancel", n, got)
	}
}
