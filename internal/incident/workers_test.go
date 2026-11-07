package incident_test

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/minz1/mediafixer/internal/incident"
)

func TestWorkers_RefuseAfterStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := incident.NewWorkersForTest(context.Background())
		ran := make(chan struct{})
		if !w.GoNamed("before", func(context.Context) { close(ran) }) {
			t.Fatal("goNamed refused before stop")
		}
		<-ran
		w.Stop()
		if w.GoNamed("after", func(context.Context) { t.Error("a refused worker ran") }) {
			t.Error("goNamed accepted after stop")
		}
		if _, ok := w.Enter("after"); ok {
			t.Error("enter accepted after stop")
		}
		if names := w.Wait(context.Background()); len(names) != 0 {
			t.Errorf("stragglers = %v, want none", names)
		}
	})
}

func TestWorkers_WaitNamesStragglersAtDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := incident.NewWorkersForTest(context.Background())
		release := make(chan struct{})
		w.GoNamed("run:stuck", func(context.Context) { <-release })
		w.GoNamed("run:polite", func(ctx context.Context) { <-ctx.Done() })
		leave, ok := w.Enter("approve:held")
		if !ok {
			t.Fatal("enter refused")
		}
		w.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		names := w.Wait(ctx)
		if want := []string{"approve:held", "run:stuck"}; !slices.Equal(names, want) {
			t.Errorf("stragglers = %v, want %v", names, want)
		}
		close(release)
		leave()
		if names = w.Wait(context.Background()); len(names) != 0 {
			t.Errorf("stragglers after release = %v, want none", names)
		}
	})
}

func TestWorkers_StopDoesNotCancelEnteredSection(t *testing.T) {
	t.Parallel()
	w := incident.NewWorkersForTest(context.Background())
	leave, ok := w.Enter("approve:x")
	if !ok {
		t.Fatal("enter refused")
	}
	w.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if names := w.Wait(ctx); !slices.Equal(names, []string{"approve:x"}) {
		t.Fatalf("stragglers = %v, want the entered section", names)
	}
	done := make(chan []string)
	go func() { done <- w.Wait(context.Background()) }()
	select {
	case names := <-done:
		t.Fatalf("wait returned %v while the section was still entered", names)
	case <-time.After(20 * time.Millisecond):
	}
	leave()
	if names := <-done; len(names) != 0 {
		t.Errorf("stragglers after leave = %v, want none", names)
	}
}
