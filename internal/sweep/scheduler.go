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
	maxRetries      = 2
)

var (
	ErrRunActive = errors.New("a run is already active")
	ErrDisabled  = errors.New("the sweep is disabled; only a Dry run can start")
)

type Scheduler struct {
	r    *Runner
	d    *db.DB
	base context.Context

	mu sync.Mutex
	wg sync.WaitGroup

	cmu    sync.Mutex
	cancel context.CancelCauseFunc
}

func NewScheduler(base context.Context, r *Runner, d *db.DB) *Scheduler {
	return &Scheduler{r: r, d: d, base: base}
}

func (s *Scheduler) Runner() *Runner { return s.r }

func (s *Scheduler) Wait() { s.wg.Wait() }

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
	next, err := s.next(set, now)
	if err != nil || now.Before(next) {
		return err
	}
	grabs, err := s.d.LiveGrabsSince(s.base, slot(set, now))
	if err != nil {
		return err
	}
	set.MaxGrabs = max(0, set.MaxGrabs-grabs)
	return s.start(triggerSchedule, false, set)
}

func (s *Scheduler) RunNow(trigger string, dry bool) error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil {
		return err
	}
	if !dry && !set.Enabled {
		return ErrDisabled
	}
	return s.start(trigger, dry, set)
}

func (s *Scheduler) NextRun(now time.Time) *time.Time {
	set, err := LoadSettings(s.base, s.d)
	if err != nil || !set.Enabled {
		return nil
	}
	next, err := s.next(set, now)
	if err != nil {
		next = slot(set, now).AddDate(0, 0, week)
	}
	return &next
}

func (s *Scheduler) next(set Settings, now time.Time) (time.Time, error) {
	at := slot(set, now)
	following := at.AddDate(0, 0, week)
	if set.EnabledAt != nil && at.Before(*set.EnabledAt) {
		return following, nil
	}
	last, err := s.d.LastSweepRunStart(s.base, []string{statusPaused, statusInterrupted, statusFailed})
	if err == nil && !last.Before(at) {
		return following, nil
	}
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return time.Time{}, err
	}
	n, latest, err := s.d.RetryableSweepRunsSince(s.base, at)
	if err != nil {
		return time.Time{}, err
	}
	if n > 0 {
		at = latest.Add(time.Hour)
	}
	if n > maxRetries || at.After(following) {
		return following, nil
	}
	return at, nil
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
	ctx, cancel := context.WithCancelCause(s.base)
	if !dry {
		s.cmu.Lock()
		s.cancel = cancel
		s.cmu.Unlock()
		if cur, lerr := LoadSettings(s.base, s.d); lerr == nil && !cur.Enabled {
			cancel(ErrDisabled)
		}
	}
	s.wg.Go(func() {
		defer s.mu.Unlock()
		defer func() {
			s.cmu.Lock()
			s.cancel = nil
			s.cmu.Unlock()
			cancel(nil)
		}()
		if _, rerr := s.r.Run(ctx, trigger, dry, set); rerr != nil {
			s.r.logger().ErrorContext(s.base, "pack sweep run", "trigger", trigger, "err", rerr)
		}
	})
	return nil
}

func (s *Scheduler) CancelLive(cause error) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.cancel != nil {
		s.cancel(cause)
	}
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
