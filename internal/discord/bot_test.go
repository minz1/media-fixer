package discord_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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

func TestStart_StopEndsRetries(t *testing.T) {
	t.Parallel()
	for name, stop := range map[string]func(*discord.Bot, context.CancelFunc){
		"close":  func(b *discord.Bot, _ context.CancelFunc) { _ = b.Close() },
		"cancel": func(_ *discord.Bot, cancel context.CancelFunc) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			b := testBot(t, failingConnect(&calls))
			ctx, cancel := context.WithCancel(t.Context())
			b.Start(ctx)
			waitForCalls(t, &calls, 2)
			stop(b, cancel)
			time.Sleep(5 * time.Millisecond)
			n := calls.Load()
			time.Sleep(20 * time.Millisecond)
			if got := calls.Load(); got != n {
				t.Errorf("connect calls grew from %d to %d after %s", n, got, name)
			}
		})
	}
}

func TestStart_ConnectFinishingAfterCloseIsDropped(t *testing.T) {
	t.Parallel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var logs syncBuffer
	b, err := discord.NewForTest(slog.New(slog.NewTextHandler(&logs, nil)), func() error {
		close(entered)
		<-release
		close(done)
		return nil
	}, time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b.Start(t.Context())
	<-entered
	_ = b.Close()
	close(release)
	<-done
	time.Sleep(10 * time.Millisecond)
	if strings.Contains(logs.String(), "discord bot started") {
		t.Errorf("logged a start after Close: %s", logs.String())
	}
}

func TestStart_BackoffDoublesUpToCap(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var at []time.Time
	b, err := discord.NewForTest(slog.New(slog.DiscardHandler), func() error {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return errors.New("down")
	}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b.Start(t.Context())
	defer func() { _ = b.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(at)
		mu.Unlock()
		if n >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d connect calls", n)
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if gap := at[2].Sub(at[1]); gap < 10*time.Millisecond {
		t.Errorf("second wait = %v, want it doubled to at least 10ms", gap)
	}
	if gap := at[4].Sub(at[3]); gap < 20*time.Millisecond || gap > 500*time.Millisecond {
		t.Errorf("fourth wait = %v, want the 20ms cap", gap)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
