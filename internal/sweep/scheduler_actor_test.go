package sweep_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/sweep"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func holdPost(t *testing.T, h *harness) (chan struct{}, chan struct{}) {
	entered, release := make(chan struct{}), make(chan struct{})
	enter := sync.OnceFunc(func() { close(entered) })
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	h.fake.beforePost = func() {
		enter()
		<-release
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return entered, release
}

func TestSweepActor_TickerChannelDrivesSameHandler(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.setNow(t0())
	if err := sweep.SaveSettings(t.Context(), h.db, settings(), t0()); err != nil {
		t.Fatal(err)
	}
	sch, ticks, _ := startedScheduler(t, h.runner, h.db)
	entered, release := holdPost(t, h)
	due := t0().Add(6 * time.Hour)
	h.setNow(due)
	ticks <- due
	v, err := sch.Status(t.Context())
	if err != nil || !v.Active || v.Dry || v.Trigger != "schedule" {
		t.Fatalf("Status = %+v, %v; want the scheduled live run", v, err)
	}
	<-entered
	close(release)
	sweep.WaitRunsForTest(sch)
	if runs := h.runs(t); len(runs) != 1 || runs[0].Trigger != "schedule" {
		t.Errorf("runs = %+v, want one scheduled run", runs)
	}
}

func TestSweepActor_EndedRunNeverRefusesRunNow(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	for i := range 50 {
		if err := sch.RunNow(t.Context(), "manual", false); err != nil {
			t.Fatalf("round %d: RunNow = %v", i, err)
		}
		sweep.WaitRunsForTest(sch)
	}
	if n := len(h.runs(t)); n != 50 {
		t.Errorf("runs = %d, want 50", n)
	}
}

func TestS6_5_DisableRightAfterRunNowCancels(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	entered, release := holdPost(t, h)
	if err := sch.RunNow(t.Context(), "manual", false); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := sweep.SaveSettings(t.Context(), h.db, disabled(), h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	cancelled, err := sch.CancelLive(t.Context(), sweep.ErrDisabled)
	if err != nil || !cancelled {
		t.Fatalf("CancelLive = %v, %v; want cancelled", cancelled, err)
	}
	close(release)
	sweep.WaitRunsForTest(sch)
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != sweepstate.Interrupted || runs[0].Reason != sweep.ErrDisabled.Error() {
		t.Errorf("runs = %+v, want one run interrupted with %q", runs, sweep.ErrDisabled.Error())
	}
}

func TestS1_2_NoRunStartsAfterBaseCancel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := sweep.SaveSettings(t.Context(), h.db, settings(), t0()); err != nil {
		t.Fatal(err)
	}
	sch, _, cancel := startedScheduler(t, h.runner, h.db)
	cancel(nil)
	for i := range 100 {
		if err := sch.RunNow(t.Context(), "manual", i%2 == 0); !errors.Is(err, card.ErrRestarting) {
			t.Fatalf("RunNow %d = %v, want ErrRestarting", i, err)
		}
		if err := sch.Tick(t.Context(), t0().Add(time.Duration(i+6)*time.Hour)); !errors.Is(err, card.ErrRestarting) {
			t.Fatalf("Tick %d = %v, want ErrRestarting", i, err)
		}
	}
	sweep.WaitRunsForTest(sch)
	if n := len(h.runs(t)); n != 0 {
		t.Errorf("runs = %d, want none", n)
	}
}

func TestSweepActor_DigestSendDoesNotBlockRequests(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, day0().Add(-48*time.Hour), settings())
	h.seedStuck(t, "s:1:1", 1)
	gate := make(chan struct{})
	notify := h.runner.Notify
	h.runner.Notify = func(ctx context.Context, c card.Card) error {
		if c.Title == "Stuck seasons" {
			<-gate
		}
		return notify(ctx, c)
	}
	_, release := holdPost(t, h)
	h.setNow(at(0, 9, 0))
	if err := sch.Tick(t.Context(), at(0, 9, 0)); err != nil {
		t.Fatalf("Tick = %v", err)
	}
	if err := sch.RunNow(t.Context(), "manual", true); !errors.Is(err, sweep.ErrRunActive) {
		t.Errorf("RunNow = %v, want ErrRunActive", err)
	}
	if v, err := sch.Status(t.Context()); err != nil || !v.Active {
		t.Errorf("Status = %+v, %v; want active", v, err)
	}
	close(gate)
	close(release)
	sweep.WaitRunsForTest(sch)
	if n := len(h.digests()); n != 1 {
		t.Errorf("digests = %d, want 1", n)
	}
}

func TestSweepActor_CallBeforeStartFailsFast(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sch := sweep.NewScheduler(t.Context(), h.runner, h.db)
	if err := sch.RunNow(t.Context(), "manual", true); !errors.Is(err, sweep.ErrNotStartedForTest) {
		t.Errorf("RunNow = %v, want errNotStarted", err)
	}
	if _, err := sch.CancelLive(t.Context(), sweep.ErrDisabled); !errors.Is(err, sweep.ErrNotStartedForTest) {
		t.Errorf("CancelLive = %v, want errNotStarted", err)
	}
	if _, err := sch.Status(t.Context()); !errors.Is(err, sweep.ErrNotStartedForTest) {
		t.Errorf("Status = %v, want errNotStarted", err)
	}
}

func TestA10_10_SweepWaitJoinsLoopAndRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := sweep.SaveSettings(t.Context(), h.db, settings(), t0()); err != nil {
		t.Fatal(err)
	}
	sch, _, cancel := startedScheduler(t, h.runner, h.db)
	entered, release := make(chan struct{}), make(chan struct{})
	enter := sync.OnceFunc(func() { close(entered) })
	rel := sync.OnceFunc(func() { close(release) })
	t.Cleanup(rel)
	notify := h.runner.Notify
	h.runner.Notify = func(ctx context.Context, c card.Card) error {
		enter()
		<-release
		return notify(ctx, c)
	}
	if err := sch.RunNow(t.Context(), "manual", false); err != nil {
		t.Fatal(err)
	}
	<-entered
	cancel(nil)
	if _, err := sch.Status(t.Context()); !errors.Is(err, card.ErrRestarting) {
		t.Fatalf("Status = %v, want ErrRestarting", err)
	}
	joined := make(chan struct{})
	go func() {
		sch.Wait()
		close(joined)
	}()
	select {
	case <-joined:
		t.Fatal("Wait returned while the run was held")
	case <-time.After(100 * time.Millisecond):
	}
	rel()
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never returned after the run ended")
	}
}

func TestS1_10_StatusActiveDuringFinish(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := sweep.SaveSettings(t.Context(), h.db, settings(), t0()); err != nil {
		t.Fatal(err)
	}
	h.rawExec(t, blockFinishSQL)
	entered, release := make(chan struct{}), make(chan struct{})
	enter := sync.OnceFunc(func() { close(entered) })
	sleep := h.runner.Sleep
	h.runner.Sleep = func(ctx context.Context, d time.Duration) error {
		enter()
		<-release
		return sleep(ctx, d)
	}
	sch := newSchedulerForTest(t, h.runner, h.db)
	if err := sch.RunNow(t.Context(), "manual", false); err != nil {
		t.Fatal(err)
	}
	<-entered
	if v, err := sch.Status(t.Context()); err != nil || !v.Active || v.Trigger != "manual" {
		t.Errorf("Status = %+v, %v; want the live run during its finish", v, err)
	}
	if err := sch.RunNow(t.Context(), "manual", false); !errors.Is(err, sweep.ErrRunActive) {
		t.Errorf("RunNow = %v, want ErrRunActive", err)
	}
	h.rawExec(t, dropBlockSQL)
	close(release)
	sweep.WaitRunsForTest(sch)
	if runs := h.runs(t); len(runs) != 1 || runs[0].Status != sweepstate.Done {
		t.Errorf("runs = %+v, want one done", runs)
	}
}
