package stuckqueue_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func slotEventsFor(k stuckqueue.SlotKind) []stuckqueue.SlotEvent {
	var out []stuckqueue.SlotEvent
	for _, h := range stuckqueue.SlotHolders() {
		if k == stuckqueue.SlotClaim && h == stuckqueue.HolderNone {
			continue
		}
		out = append(out, stuckqueue.SlotEventOf(k, h))
	}
	return out
}

func slotNext(h stuckqueue.Holder, ev stuckqueue.SlotEvent) (stuckqueue.Holder, error) {
	return stuckqueue.SlotNext(h, ev)
}

func slotMatches(h stuckqueue.Holder, ev stuckqueue.SlotEvent) int {
	next, err := stuckqueue.SlotNext(h, ev)
	if err == nil && next != h && next != stuckqueue.HolderNone && next != ev.Holder {
		return 0
	}
	if err != nil && next != h {
		return 0
	}
	return 1
}

func slotCheck(h stuckqueue.Holder) error {
	if !slices.Contains(stuckqueue.SlotHolders(), h) {
		return fmt.Errorf("holder %q is not one of the four", h)
	}
	return nil
}

func slotObligations(before stuckqueue.Holder, ev stuckqueue.SlotEvent) error {
	next, err := stuckqueue.SlotNext(before, ev)
	return slotOwes(before, ev, next, err)
}

func errText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

func slotOwes(before stuckqueue.Holder, ev stuckqueue.SlotEvent, next stuckqueue.Holder, err error) error {
	switch ev.Kind {
	case stuckqueue.SlotClaim:
		switch {
		case before == stuckqueue.HolderNone && (err != nil || next != ev.Holder):
			return fmt.Errorf("claim of a free slot gave %q, %s", next, errText(err))
		case before != stuckqueue.HolderNone && err == nil:
			return fmt.Errorf("claim of a slot held by %q was accepted", before)
		case before != stuckqueue.HolderNone && err.Error() != stuckqueue.Refusal(before, ev.Holder).Error():
			return fmt.Errorf("refusal text %q is not the table's", errText(err))
		}
	case stuckqueue.SlotRelease:
		switch {
		case before == stuckqueue.HolderNone && !errors.Is(err, stuckqueue.ErrNotHeld):
			return fmt.Errorf("release of a free slot gave %s, want not_held", errText(err))
		case before != stuckqueue.HolderNone && before != ev.Holder && !errors.Is(err, stuckqueue.ErrWrongHolder):
			return fmt.Errorf("release by %q of %q gave %s, want wrong_holder", ev.Holder, before, errText(err))
		case before != stuckqueue.HolderNone && before == ev.Holder && (err != nil || next != stuckqueue.HolderNone):
			return fmt.Errorf("release by the holder gave %q, %s", next, errText(err))
		}
	}
	return nil
}

func slotMachine() machinetest.Machine[stuckqueue.Holder, stuckqueue.SlotKind, stuckqueue.Holder, stuckqueue.SlotEvent] {
	return machinetest.Machine[stuckqueue.Holder, stuckqueue.SlotKind, stuckqueue.Holder, stuckqueue.SlotEvent]{
		States:   stuckqueue.SlotHolders(),
		Kinds:    stuckqueue.SlotKinds(),
		Initial:  stuckqueue.HolderNone,
		State:    func(h stuckqueue.Holder) stuckqueue.Holder { return h },
		Key:      func(h stuckqueue.Holder) string { return string(h) },
		Samples:  func(h stuckqueue.Holder) []stuckqueue.Holder { return []stuckqueue.Holder{h} },
		Events:   slotEventsFor,
		Matches:  slotMatches,
		Next:     slotNext,
		Check:    slotCheck,
		Terminal: func(stuckqueue.Holder) bool { return false },
		Extra:    slotObligations,
	}
}

func TestStuckSlot_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, slotMachine())
	for _, h := range stuckqueue.SlotHolders() {
		for _, k := range stuckqueue.SlotKinds() {
			for _, ev := range slotEventsFor(k) {
				next, nerr := stuckqueue.SlotNext(h, ev)
				if err := slotOwes(h, ev, next, nerr); err != nil {
					t.Errorf("holder %s, event %+v: %v", h, ev, err)
				}
			}
		}
	}
}

func TestStuckSlot_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, slotMachine())
}

func TestStuckSlot_Invariants(t *testing.T) {
	t.Parallel()
	machinetest.Invariants(t, slotMachine())
}

func FuzzStuckSlot(f *testing.F) {
	m := slotMachine()
	for i := range 8 {
		f.Add([]byte{byte(i)})
	}
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6})
	f.Add([]byte{1, 0, 3, 2, 5, 4})
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func TestStuckSlot_RefusalTextTable(t *testing.T) {
	t.Parallel()
	const (
		runText   = "a stuck-queue run is already running"
		ownerRun  = "a stuck-queue run is active, try again in a moment"
		ownerText = "an owner action is running, try again in a moment"
		houseText = "a background check is running, try again in a moment"
	)
	holders := stuckqueue.SlotHolders()
	want := map[[2]stuckqueue.Holder]string{}
	for _, c := range holders {
		want[[2]stuckqueue.Holder{stuckqueue.HolderRun, c}] = ownerRun
		want[[2]stuckqueue.Holder{stuckqueue.HolderOwner, c}] = ownerText
		want[[2]stuckqueue.Holder{stuckqueue.HolderHousekeeping, c}] = houseText
	}
	want[[2]stuckqueue.Holder{stuckqueue.HolderRun, stuckqueue.HolderRun}] = runText
	for _, h := range holders {
		for _, c := range holders {
			err := stuckqueue.Refusal(h, c)
			if h == stuckqueue.HolderNone {
				if err != nil {
					t.Errorf("holder none, claimant %s: refusal %v, want none", c, err)
				}
				continue
			}
			if err == nil || err.Error() != want[[2]stuckqueue.Holder{h, c}] {
				t.Errorf("holder %s, claimant %s: %v, want %q", h, c, err, want[[2]stuckqueue.Holder{h, c}])
			}
		}
	}
	if !errors.Is(stuckqueue.Refusal(stuckqueue.HolderRun, stuckqueue.HolderRun), stuckqueue.ErrRunActive) {
		t.Error("run x RunNow is not ErrRunActive")
	}
	if !errors.Is(stuckqueue.Refusal(stuckqueue.HolderOwner, stuckqueue.HolderRun), stuckqueue.ErrOwnerActing) {
		t.Error("owner x RunNow is not ErrOwnerActing")
	}
	if !errors.Is(stuckqueue.Refusal(stuckqueue.HolderOwner, stuckqueue.HolderOwner), stuckqueue.ErrRefused) {
		t.Error("an owner-action refusal is not an ErrRefused")
	}
}

const (
	textOwnerActing = "an owner action is running, try again in a moment"
	textRunActive   = "a stuck-queue run is active, try again in a moment"
)

func stuckD(e *runEnv) (string, error) {
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if err := seedSeen(e, stuckqueue.AppSonarr, "D", e.clock.Now().Add(-7*time.Hour)); err != nil {
		return "", err
	}
	entries, err := e.sonarr.client().QueueEntries(context.Background())
	if err != nil {
		return "", err
	}
	for _, d := range stuckqueue.Group(stuckqueue.AppSonarr, entries) {
		if d.DownloadID == "D" {
			return d.TargetsFP(), nil
		}
	}
	return "", errors.New("D is not in the queue")
}

func TestQ3_3_TickWhileHeldSkipsAndKeepsLastStart(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	started := s.StartedAt()
	e.sonarr.awaitArrived(t, 1)
	e.clock.Add(3 * time.Hour)
	s.Tick(e.clock.Now())
	if got := s.StartedAt(); !got.Equal(started) {
		t.Errorf("StartedAt = %v after a held tick, want %v", got, started)
	}
	close(gate)
	s.Wait()
	if n := strings.Count(e.logs.String(), "level=ERROR"); n != 0 {
		t.Errorf("a held tick logged %d errors:\n%s", n, e.logs.String())
	}
}

func TestQ3_4_RunNowDuringOwnerActionSaysSo(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	fp, err := stuckD(e)
	if err != nil {
		t.Fatal(err)
	}
	gate := e.sonarr.hold()
	n := e.sonarr.requests() + 1
	done := make(chan error, 1)
	go func() {
		_, aerr := s.Act(context.Background(), stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, fp)
		done <- aerr
	}()
	e.sonarr.awaitArrived(t, n)
	got := s.RunNow()
	close(gate)
	if aerr := <-done; aerr != nil {
		t.Fatalf("Act: %v", aerr)
	}
	if !errors.Is(got, stuckqueue.ErrOwnerActing) || got.Error() != textOwnerActing {
		t.Fatalf("RunNow during Act = %v, want %q", got, textOwnerActing)
	}
}

func TestQ3_7_OwnerActionDuringRunGetsRunText(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, actErr := s.Act(ctx, stuckqueue.AppRadarr, "M", stuckqueue.ActionRemove, "fp")
	_, impErr := s.ImportMapped(ctx, 1, "D", "x", bothRows())
	_, resErr := s.Resume(ctx, stuckqueue.AppSonarr, "D", 1, 0)
	close(gate)
	s.Wait()
	for name, err := range map[string]error{"Act": actErr, "ImportMapped": impErr, "Resume": resErr} {
		if !errors.Is(err, stuckqueue.ErrRefused) || !strings.HasSuffix(err.Error(), textRunActive) {
			t.Errorf("%s during a run = %v, want %q", name, err, textRunActive)
		}
	}
	if err := s.RunNow(); err != nil {
		t.Errorf("RunNow after the run: %v", err)
	}
	s.Wait()
}

func TestStuckSlot_RunningDerivedFromHolder(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	if _, has, running := s.State(); !running || has {
		t.Fatalf("with a run blocked, State = has %v, running %v", has, running)
	}
	close(gate)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res, has, running := s.State()
		if running {
			time.Sleep(time.Millisecond)
			continue
		}
		if !has || res.FinishedAt.IsZero() {
			t.Fatalf("State showed not running without the new last: has %v, %+v", has, res)
		}
		s.Wait()
		return
	}
	t.Fatal("the run never released")
}

func TestStuckSlot_ReleaseIsOnce(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	now := time.Now()
	first, err := stuckqueue.ClaimForTest(s, stuckqueue.HolderOwner, now)
	if err != nil {
		t.Fatal(err)
	}
	first()
	second, err := stuckqueue.ClaimForTest(s, stuckqueue.HolderOwner, now)
	if err != nil {
		t.Fatalf("claim after a release: %v", err)
	}
	first()
	if _, err = stuckqueue.ClaimForTest(s, stuckqueue.HolderOwner, now); !errors.Is(err, stuckqueue.ErrOwnerActing) {
		t.Fatalf("a second release of the first claim freed the second: %v", err)
	}
	second()
	third, err := stuckqueue.ClaimForTest(s, stuckqueue.HolderRun, now)
	if err != nil {
		t.Fatalf("claim after both releases: %v", err)
	}
	third()
}

func requireRunNowFree(t *testing.T, s *stuckqueue.Scheduler) {
	t.Helper()
	if err := s.RunNow(); err != nil {
		t.Fatalf("RunNow after the action: %v", err)
	}
	s.Wait()
}

func TestStuckSlot_ReleaseOnEveryActPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bad := "0123456789ab"
	cases := map[string]func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error{
		"not configured": func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			e.runner.Radarr = nil
			_, err := s.Act(ctx, stuckqueue.AppRadarr, "M", stuckqueue.ActionRemove, bad)
			return err
		},
		"not in queue": func(_ *testing.T, _ *runEnv, s *stuckqueue.Scheduler) error {
			_, err := s.Act(ctx, stuckqueue.AppSonarr, "GONE", stuckqueue.ActionRemove, bad)
			return err
		},
		"not stuck": func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			busy := rawRec(2, "D", "Pack", 2, false)
			busy["status"], busy["trackedDownloadState"] = "downloading", "downloading"
			e.sonarr.set(rawRec(1, "D", "Pack", 1, false, sample), busy)
			_, err := s.Act(ctx, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, bad)
			return err
		},
		"fingerprint": func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
			_, err := s.Act(ctx, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, bad)
			return err
		},
		"not offered": func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			e.sonarr.set(rawRec(1, "D", "AHS", 12, true, sample))
			_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport)
			return err
		},
		"sonarr error": func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			e.sonarr.mu.Lock()
			e.sonarr.failQueue = true
			e.sonarr.mu.Unlock()
			_, err := s.Act(ctx, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, bad)
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, s := newSched(t)
			if err := run(t, e, s); err == nil {
				t.Fatal("the action succeeded, want a refusal or error")
			}
			requireRunNowFree(t, s)
		})
	}
}

func TestStuckSlot_ReleaseOnEveryImportMappedPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := map[string]func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error{
		"not configured": func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			e.runner.Sonarr = nil
			_, err := s.ImportMapped(ctx, 1, "D", "x", bothRows())
			return err
		},
		"queue entries": func(_ *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			e.sonarr.mu.Lock()
			e.sonarr.failQueue = true
			e.sonarr.mu.Unlock()
			_, err := s.ImportMapped(ctx, 1, "D", "x", bothRows())
			return err
		},
		"owned stuck": func(_ *testing.T, _ *runEnv, s *stuckqueue.Scheduler) error {
			_, err := s.ImportMapped(ctx, 1, "D", "stale", bothRows())
			return err
		},
		"candidates": func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			fp := targetsFp(t, e)
			e.sonarr.setCandidates("D", "{not json")
			_, err := s.ImportMapped(ctx, 1, "D", fp, bothRows())
			return err
		},
		"episodes": func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			fp := targetsFp(t, e)
			e.sonarr.setFailEpisodes()
			_, err := s.ImportMapped(ctx, 1, "D", fp, bothRows())
			return err
		},
		"pending read": func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			fp := targetsFp(t, e)
			e.sonarr.setCommands(500)
			_, err := s.ImportMapped(ctx, 1, "D", fp, bothRows())
			return err
		},
		"already pending": func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			fp := targetsFp(t, e)
			e.sonarr.setCommands(0, map[string]any{
				"name": "ManualImport", "status": "started",
				"body": map[string]any{"files": []map[string]any{{"path": pathB}}},
			})
			_, err := s.ImportMapped(ctx, 1, "D", fp, bothRows())
			return err
		},
		"check mapping": func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			fp := targetsFp(t, e)
			_, err := s.ImportMapped(ctx, 1, "D", fp, importRows(
				stuckqueue.MappedFile{Path: "/elsewhere/x.mkv", EpisodeIDs: []int{1}},
			))
			return err
		},
		"manual import": func(t *testing.T, e *runEnv, s *stuckqueue.Scheduler) error {
			fp := targetsFp(t, e)
			e.sonarr.mu.Lock()
			e.sonarr.failCommand["ManualImport"] = true
			e.sonarr.mu.Unlock()
			_, err := s.ImportMapped(ctx, 1, "D", fp, bothRows())
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, s := importEnv(t)
			if err := run(t, e, s); err == nil {
				t.Fatal("the import succeeded, want a refusal or error")
			}
			requireRunNowFree(t, s)
		})
	}
}

func answers(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not answer while an Act call was in flight", what)
	}
}

func TestStuckSlot_StateAnswersDuringActCall(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	fp, err := stuckD(e)
	if err != nil {
		t.Fatal(err)
	}
	gate := e.sonarr.hold()
	n := e.sonarr.requests() + 1
	done := make(chan error, 1)
	go func() {
		_, aerr := s.Act(context.Background(), stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, fp)
		done <- aerr
	}()
	e.sonarr.awaitArrived(t, n)
	answers(t, "State", func() { s.State() })
	answers(t, "StartedAt", func() { s.StartedAt() })
	answers(t, "the card renderer", func() {
		_, _ = s.CardRenderer()(context.Background(), card.StuckSubject("s", "D"))
	})
	close(gate)
	if aerr := <-done; aerr != nil {
		t.Fatalf("Act: %v", aerr)
	}
}

func TestStuckSlot_RefreshAfterRelease(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	fp, err := stuckD(e)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []string
	var calls int
	s.Refresh = func(subject string) {
		_, perr := s.Act(context.Background(), stuckqueue.AppRadarr, "PROBE", stuckqueue.ActionRemove, "fp")
		mu.Lock()
		defer mu.Unlock()
		calls++
		if perr == nil || !strings.Contains(perr.Error(), "no longer in the Radarr queue") {
			held = append(held, fmt.Sprintf("%s: %v", subject, perr))
		}
	}
	if _, err = s.Act(context.Background(), stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, fp); err != nil {
		t.Fatal(err)
	}
	if err = s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("Refresh called %d times, want one per action and one per run", calls)
	}
	if len(held) != 0 {
		t.Fatalf("Refresh ran with the slot held: %q", held)
	}
}

func TestGuard_RestartingRefusedBeforeLock(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	base, cancel := context.WithCancel(context.Background())
	s := stuckqueue.NewScheduler(base, e.runner, e.db)
	t.Cleanup(s.Wait)
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	cancel()
	ctx := context.Background()
	_, actErr := s.Act(ctx, stuckqueue.AppRadarr, "M", stuckqueue.ActionRemove, "fp")
	_, impErr := s.ImportMapped(ctx, 1, "D", "x", bothRows())
	_, resErr := s.Resume(ctx, stuckqueue.AppSonarr, "D", 1, 0)
	close(gate)
	s.Wait()
	for name, err := range map[string]error{"Act": actErr, "ImportMapped": impErr, "Resume": resErr} {
		if !errors.Is(err, card.ErrRestarting) {
			t.Errorf("%s while closing and held = %v, want ErrRestarting", name, err)
		}
	}
}

func TestSlotMachine_Idempotence(t *testing.T) {
	t.Parallel()
	machinetest.Idempotence(t, slotMachine(), []stuckqueue.SlotKind{stuckqueue.SlotRelease})
}
