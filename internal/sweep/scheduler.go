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

	wmu    sync.Mutex
	warned string
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
	err := s.tick(now)
	if errors.Is(err, ErrRunActive) || errors.Is(err, ErrPaused) {
		err = nil
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	switch {
	case err != nil && err.Error() != s.warned:
		s.warned = err.Error()
		s.r.logger().WarnContext(s.base, "pack sweep scheduler", "err", err)
	case err == nil && s.warned != "":
		s.warned = ""
		s.r.logger().InfoContext(s.base, "pack sweep scheduler error cleared")
	}
}

func (s *Scheduler) tick(now time.Time) error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil || !set.Enabled {
		return err
	}
	runErr := s.runDue(set, now)
	if errors.Is(runErr, ErrRunActive) || errors.Is(runErr, ErrPaused) {
		runErr = nil
	}
	return errors.Join(s.digest(set, now), runErr)
}

func (s *Scheduler) runDue(set Settings, now time.Time) error {
	next, err := s.next(set, now)
	if err != nil || now.Before(next) {
		return err
	}
	u, err := s.Used(now)
	if err != nil {
		return err
	}
	return s.start(triggerSchedule, false, Remaining(set, u))
}

func (s *Scheduler) RunNow(trigger string, dry bool) error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil {
		return err
	}
	if !dry && !set.Enabled {
		return ErrDisabled
	}
	if !dry {
		u, uerr := s.Used(s.r.Now())
		if uerr != nil {
			return uerr
		}
		set = Remaining(set, u)
	}
	return s.start(trigger, dry, set)
}

type Usage struct {
	Grabs, Lookups, Searches int
	Crashed                  bool
}

func (s *Scheduler) Used(now time.Time) (Usage, error) {
	since := now.Add(-24 * time.Hour)
	var u Usage
	var err error
	if u.Grabs, err = s.d.LiveGrabsSince(s.base, since); err != nil {
		return u, err
	}
	if u.Lookups, err = s.d.LiveLookupsSince(s.base, since); err != nil {
		return u, err
	}
	u.Searches, u.Crashed, err = s.d.LiveSearchesSince(s.base, since)
	return u, err
}

func Remaining(set Settings, u Usage) Settings {
	set.MaxGrabs = max(0, set.MaxGrabs-u.Grabs)
	set.MaxLookups = max(0, set.MaxLookups-u.Lookups)
	set.MaxSearches = max(0, set.MaxSearches-u.Searches)
	if u.Crashed {
		set.MaxSearches = 0
	}
	return set
}

func (s *Scheduler) NextRun(now time.Time) *time.Time {
	set, err := LoadSettings(s.base, s.d)
	if err != nil || !set.Enabled {
		return nil
	}
	next, err := s.next(set, now)
	if err != nil {
		next = now.Add(set.Interval())
	}
	return &next
}

func (s *Scheduler) next(set Settings, now time.Time) (time.Time, error) {
	iv := set.Interval()
	var anchor time.Time
	last, err := s.d.LastSweepRunStart(s.base, []string{statusPaused, statusInterrupted, statusFailed})
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return time.Time{}, err
	}
	if last != nil {
		anchor = *last
	}
	if set.EnabledAt != nil && set.EnabledAt.After(anchor) {
		anchor = *set.EnabledAt
	}
	var at time.Time
	switch {
	case anchor.IsZero():
		at = now.Truncate(iv)
	case now.Before(anchor.Add(iv)):
		return anchor.Add(iv), nil
	default:
		at = anchor.Add(now.Sub(anchor).Truncate(iv))
	}
	following := at.Add(iv)
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
