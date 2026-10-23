package sweep

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

const (
	triggerSchedule = "schedule"
	week            = 7
)

var ErrRunActive = errors.New("a run is already active")

type Scheduler struct {
	r    *Runner
	d    *db.DB
	base context.Context

	mu sync.Mutex
	wg sync.WaitGroup
}

func NewScheduler(base context.Context, r *Runner, d *db.DB) *Scheduler {
	return &Scheduler{r: r, d: d, base: base}
}

func (s *Scheduler) Runner() *Runner { return s.r }

func (s *Scheduler) Start() {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.base.Done():
				return
			case <-t.C:
				s.Tick(s.r.Now())
			}
		}
	}()
}

func (s *Scheduler) Tick(now time.Time) {
	if err := s.tick(now); err != nil && !errors.Is(err, ErrRunActive) && !errors.Is(err, ErrPaused) {
		s.r.logger().WarnContext(s.base, "pack sweep scheduler", "err", err)
	}
}

func (s *Scheduler) tick(now time.Time) error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil || !set.Enabled {
		return err
	}
	due, err := s.due(set, now)
	if err != nil || !due {
		return err
	}
	return s.start(triggerSchedule, false, set)
}

func (s *Scheduler) RunNow(trigger string, dry bool) error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil {
		return err
	}
	return s.start(trigger, dry, set)
}

func (s *Scheduler) NextRun(now time.Time) *time.Time {
	set, err := LoadSettings(s.base, s.d)
	if err != nil || !set.Enabled {
		return nil
	}
	next := slot(set, now)
	if due, derr := s.due(set, now); derr != nil || !due {
		next = next.AddDate(0, 0, week)
	}
	return &next
}

func (s *Scheduler) due(set Settings, now time.Time) (bool, error) {
	last, err := s.d.LastSweepRunStart(s.base, []string{statusPaused, statusInterrupted})
	if errors.Is(err, db.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return last.Before(slot(set, now)), nil
}

func (s *Scheduler) start(trigger string, dry bool, set Settings) error {
	if !s.mu.TryLock() {
		return ErrRunActive
	}
	paused, err := s.d.IsAutonomousPaused(s.base)
	if err == nil && paused {
		err = ErrPaused
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.wg.Go(func() {
		defer s.mu.Unlock()
		if _, rerr := s.r.Run(s.base, trigger, dry, set); rerr != nil {
			s.r.logger().ErrorContext(s.base, "pack sweep run", "trigger", trigger, "err", rerr)
		}
	})
	return nil
}

func slot(s Settings, now time.Time) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), s.HourUTC, 0, 0, 0, time.UTC)
	at = at.AddDate(0, 0, -((int(now.Weekday()) - s.Weekday + week) % week))
	if at.After(now) {
		at = at.AddDate(0, 0, -week)
	}
	return at
}

func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
