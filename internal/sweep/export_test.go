package sweep

import (
	"context"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func SummaryCardForTest(sr db.SweepRun) card.Card {
	return summaryCard(sr)
}

func SetProgressForTest(r *Runner, p *Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = p
}

func CountsTextForTest(c db.SweepCounts, dry bool) string { return countsText(c, dry) }

func StartCardForTest(id int64, trigger string, dry bool, s Settings) card.Card {
	return startCard(id, trigger, dry, s)
}

func NextEligibleForTest(last time.Time, streak int, s Settings) time.Time {
	return nextEligible(last, streak, s)
}

func AttemptTargetForTest(
	prev *db.SweepTarget, key string, series client.Series, season, missing int, now time.Time, s Settings,
) db.SweepTarget {
	return attemptTarget(prev, key, series, season, missing, now, s)
}

func EnabledAtForTest(prev, next Settings, now time.Time) *time.Time {
	return enabledAt(prev, next, now)
}

func DigestCardForTest(stuck []db.SweepTarget) card.Card { return digestCard(stuck) }

func (r *Runner) Run(ctx context.Context, trigger string, dry bool, s Settings) (int64, error) {
	return r.runScoped(ctx, trigger, dry, s, scope{})
}

func OutcomeForTest(err, ctxErr error) (sweepstate.Outcome, string) { return outcome(err, ctxErr) }

func ErrRedownloadForTest() error { return errRedownload }

func AddItemForTest(ctx context.Context, r *Runner, dry bool, it *db.SweepItem) error {
	return (&run{Runner: r, dry: dry}).addItem(ctx, it)
}

type (
	SlotState = slotState
	SlotKind  = slotKind
	SlotSnap  = slotSnap
	SlotEvent = slotEvent
	SlotEff   = slotEff
	EffKind   = effKind
)

const (
	SlotIdle       = slotIdle
	SlotLive       = slotLive
	SlotDry        = slotDry
	SlotStopped    = slotStopped
	SlotStart      = slotStart
	SlotDone       = slotDone
	SlotCancelLive = slotCancelLive
	SlotReconcile  = slotReconcile
	SlotStop       = slotStop
	EffLaunch      = effLaunch
	EffCancel      = effCancel
	EffCrashStale  = effCrashStale
)

var (
	ErrSlotNoRun   = errSlotNoRun
	ErrSlotBusy    = errSlotBusy
	ErrSlotStopped = errSlotStopped
)

func NextSlot(cur SlotSnap, ev SlotEvent) (SlotSnap, []SlotEff, error) { return nextSlot(cur, ev) }

func CheckSlot(s SlotSnap) error { return checkSlot(s) }

func StartForTest(s *Scheduler, ticks <-chan time.Time) {
	s.started.Store(true)
	s.loopWG.Go(func() { s.loop(s.base, ticks) })
}

func WaitRunsForTest(s *Scheduler) { s.jobs.Wait() }

var ErrNotStartedForTest = errNotStarted

func ReportForTest(r *Runner, id int64, start time.Time) func(phase, current string) {
	return (&run{Runner: r, id: id, start: start, refreshed: start}).report
}

func PauseForTest(ctx context.Context, r *Runner, d time.Duration) error {
	wake := make(chan struct{}, 1)
	r.wake.Store(&wake)
	defer r.wake.Store(nil)
	return (&run{Runner: r, wake: wake}).pause(ctx, d)
}
