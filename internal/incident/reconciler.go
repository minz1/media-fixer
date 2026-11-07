package incident

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func (s *Service) Start() {
	if s.work.goNamed("reconciler", s.reconcileLoop) {
		s.log.Info("incident sweep loop started")
	}
}

func (s *Service) StartRecovery() {
	s.work.goNamed("recovery", func(ctx context.Context) {
		c := s.reconcile(ctx)
		s.log.InfoContext(ctx, fmt.Sprintf("reconciler: startup runs=%d checks=%d plans_lost=%d parked=%d",
			c.Runs, c.Checks, c.PlansLost, c.Parked))
	})
}

func (s *Service) wakeReconciler() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) reconcileLoop(base context.Context) {
	d := s.minutes(s.timers(base).CheckEveryMinutes)
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-base.Done():
			return
		case <-s.wake:
			s.reconcile(base)
		case <-ticker.C:
			s.reconcile(base)
			s.AdvanceReporterProgress(base, time.Now())
			s.endLockEpisode(base)
			if next := s.minutes(s.timers(base).CheckEveryMinutes); next != d {
				ticker.Reset(next)
				d = next
			}
		}
	}
}

func (s *Service) reconcile(ctx context.Context) lifecycle.Counts {
	leave, ok := s.work.enter("reconcile")
	if !ok {
		s.log.InfoContext(ctx, "reconciler: pass refused", "error", card.ErrRestarting)
		return lifecycle.Counts{}
	}
	defer leave()
	rows, err := s.db.ListDriven(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "reconciler: list driven incidents", "error", err)
		}
		return lifecycle.Counts{}
	}
	t := s.timers(ctx)
	live := s.runs.liveness()
	floors := make(map[string]time.Duration, len(live.floors))
	for id, m := range live.floors {
		floors[id] = s.minutes(m)
	}
	paused := s.paused.Load()
	intents := lifecycle.Reconcile(rows, lifecycle.Liveness{
		Runs: live.runs, Reserved: live.reserved, Plans: live.plans, Armed: live.armed, Floors: floors,
	}, lifecycle.Policy{
		Now: time.Now(), Paused: paused, StaleAfter: s.minutes(t.StaleRunMinutes), Unit: s.minutes(1),
	})
	for _, in := range intents {
		if ctx.Err() != nil {
			break
		}
		s.drive(ctx, in)
	}
	return lifecycle.Tally(intents, paused)
}

func (s *Service) drive(ctx context.Context, in lifecycle.Intent) {
	if in.Arm != nil {
		s.scheduleCheck(in.ID, *in.Arm)
		return
	}
	if in.Event.Kind == lifecycle.PlanLost && s.runs.liveness().plans[in.ID] {
		return
	}
	_, err := s.apply(ctx, in.ID, *in.Event, nil)
	switch {
	case err == nil:
	case errors.Is(err, lifecycle.ErrStale), errors.Is(err, lifecycle.ErrParked), errors.Is(err, lifecycle.ErrNoop),
		errors.Is(err, lifecycle.ErrPaused), errors.Is(err, lifecycle.ErrNotParked), ctx.Err() != nil:
		s.log.InfoContext(ctx, "reconciler: decision lost a race", "incident", in.ID,
			"event", string(in.Event.Kind), "error", err)
	default:
		s.log.ErrorContext(ctx, "reconciler: apply", "incident", in.ID, "event", string(in.Event.Kind), "error", err)
	}
}
