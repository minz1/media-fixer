package stuckqueue_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func newSched(t *testing.T) (*runEnv, *stuckqueue.Scheduler) {
	t.Helper()
	e := newRunEnv(t)
	s := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s.Wait)
	return e, s
}

func (e *runEnv) tickAt(s *stuckqueue.Scheduler, after time.Duration) time.Time {
	e.clock.Add(after)
	now := e.clock.Now()
	s.Tick(now)
	s.Wait()
	return now
}

func ranAt(t *testing.T, s *stuckqueue.Scheduler, at time.Time) bool {
	t.Helper()
	res, ok := s.Last()
	return ok && res.FinishedAt.Equal(at)
}

func TestTick_FirstTickRuns(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	if at := e.tickAt(s, time.Minute); !ranAt(t, s, at) {
		t.Fatal("the first tick must run")
	}
}

func TestTick_RespectsInterval(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.tickAt(s, time.Minute)
	if at := e.tickAt(s, 59*time.Minute); ranAt(t, s, at) {
		t.Fatal("ran again inside the interval")
	}
	if at := e.tickAt(s, time.Minute); !ranAt(t, s, at) {
		t.Fatal("did not run once the interval passed")
	}
}

func TestTick_IntervalChangeApplies(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.tickAt(s, time.Minute)
	set := stuckqueue.DefaultSettings()
	set.IntervalMinutes = 15
	if err := stuckqueue.SaveSettings(context.Background(), e.db, set); err != nil {
		t.Fatal(err)
	}
	if at := e.tickAt(s, 15*time.Minute); !ranAt(t, s, at) {
		t.Fatal("the new interval did not apply")
	}
}

func TestTick_DisabledNoRun(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	set := stuckqueue.DefaultSettings()
	set.Enabled = false
	if err := stuckqueue.SaveSettings(context.Background(), e.db, set); err != nil {
		t.Fatal(err)
	}
	e.tickAt(s, time.Minute)
	if _, ok := s.Last(); ok {
		t.Fatal("a disabled sweep ran")
	}
}

func TestTick_CorruptWarnsOnce(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	if err := e.db.SetSetting(context.Background(), stuckqueue.SettingsKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.tickAt(s, time.Minute)
	}
	if n := strings.Count(e.logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("WARNs = %d, want 1:\n%s", n, e.logs.String())
	}
	if _, ok := s.Last(); ok {
		t.Fatal("ran with corrupt settings")
	}
}

func TestRunNow_SingleFlight(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow(); !errors.Is(err, stuckqueue.ErrRunActive) {
		t.Fatalf("second RunNow = %v, want ErrRunActive", err)
	}
	close(gate)
	s.Wait()
}

func TestLast_AfterRun(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	if _, ok := s.Last(); ok {
		t.Fatal("Last before any run")
	}
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	res, ok := s.Last()
	if !ok || len(res.Apps) != 2 {
		t.Fatalf("last = %+v, %v", res, ok)
	}
}

func TestRunNow_Disabled(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	set := stuckqueue.DefaultSettings()
	set.Enabled = false
	if err := stuckqueue.SaveSettings(context.Background(), e.db, set); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow(); !errors.Is(err, stuckqueue.ErrDisabled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunNow_Corrupt(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	if err := e.db.SetSetting(context.Background(), stuckqueue.SettingsKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow(); !errors.Is(err, stuckqueue.ErrCorruptSettings) {
		t.Fatalf("err = %v", err)
	}
}

func TestScheduler_RefreshesAfterStoringLast(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	var mu sync.Mutex
	var calls []string
	var rendered []bool
	s.Refresh = func(prefix string) {
		_, ok, _ := s.CardRenderer()(context.Background(), card.StuckSubject("s", "D"))
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, prefix)
		rendered = append(rendered, ok)
	}
	e.tickAt(s, time.Minute)
	e.tickAt(s, 6*time.Hour)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{"sq:", "sq:"}) {
		t.Fatalf("refresh calls = %v, want one per run", calls)
	}
	if !rendered[1] {
		t.Fatal("the run's own item did not render in its post-run refresh")
	}
	up, ok, _ := s.CardRenderer()(context.Background(), card.StuckSubject("s", "D"))
	if !ok || len(up.Buttons) == 0 || up.Status != "" {
		t.Fatalf("update = %+v, %v; want still stuck with buttons", up, ok)
	}
}

func TestState_ResultAndRunningTogether(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	if _, _, running := s.State(); running {
		t.Fatal("running before any run")
	}
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	if _, ok, running := s.State(); ok || !running {
		t.Fatalf("held run: ok=%v running=%v, want no result and running", ok, running)
	}
	close(gate)
	s.Wait()
	if res, ok, running := s.State(); !ok || running || len(res.Apps) != 2 {
		t.Fatalf("finished run: ok=%v running=%v apps=%d", ok, running, len(res.Apps))
	}
}
