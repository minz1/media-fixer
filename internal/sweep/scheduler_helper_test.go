package sweep_test

import (
	"context"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func startedScheduler(
	t *testing.T, r *sweep.Runner, d *db.DB,
) (*sweep.Scheduler, chan time.Time, context.CancelCauseFunc) {
	t.Helper()
	base, cancel := context.WithCancelCause(t.Context())
	sch := sweep.NewScheduler(base, r, d)
	ticks := make(chan time.Time)
	sweep.StartForTest(sch, ticks)
	t.Cleanup(func() {
		cancel(nil)
		sch.Wait()
	})
	return sch, ticks, cancel
}

func newSchedulerForTest(t *testing.T, r *sweep.Runner, d *db.DB) *sweep.Scheduler {
	t.Helper()
	sch, _, _ := startedScheduler(t, r, d)
	return sch
}

func (h *harness) setNow(at time.Time) {
	h.clock.mu.Lock()
	h.clock.t = at
	h.clock.mu.Unlock()
}
