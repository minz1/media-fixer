package stuckqueue

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

var (
	ErrRunActive = errors.New("a stuck-queue run is already running")
	ErrDisabled  = errors.New("the stuck-queue sweep is disabled")
)

type Scheduler struct {
	Refresh func(string)

	r    *Runner
	d    *db.DB
	base context.Context

	mu sync.Mutex
	wg sync.WaitGroup

	smu       sync.Mutex
	lastStart time.Time
	last      Result
	hasLast   bool
	running   bool
	warned    string
	outcomes  map[string]outcome
}

func NewScheduler(base context.Context, r *Runner, d *db.DB) *Scheduler {
	return &Scheduler{r: r, d: d, base: base}
}

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
	set, err := LoadSettings(s.base, s.d)
	s.warnOnce(err)
	if err != nil || !set.Enabled {
		return
	}
	s.smu.Lock()
	due := s.lastStart.IsZero() || now.Sub(s.lastStart) >= set.Interval()
	s.smu.Unlock()
	if due {
		_ = s.start(now, set)
	}
}

func (s *Scheduler) RunNow() error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil {
		return err
	}
	if !set.Enabled {
		return ErrDisabled
	}
	return s.start(s.r.Now(), set)
}

func (s *Scheduler) Last() (Result, bool) {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.last, s.hasLast
}

func (s *Scheduler) Running() bool {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.running
}

func (s *Scheduler) start(now time.Time, set Settings) error {
	if !s.mu.TryLock() {
		return ErrRunActive
	}
	s.smu.Lock()
	s.lastStart, s.running = now, true
	s.smu.Unlock()
	s.wg.Go(func() {
		defer s.mu.Unlock()
		res := s.r.Run(s.base, set)
		s.smu.Lock()
		s.last, s.hasLast, s.running = res, true, false
		s.smu.Unlock()
		s.refresh(cardPrefix)
	})
	return nil
}

func (s *Scheduler) warnOnce(err error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	switch {
	case err != nil && err.Error() != s.warned:
		s.warned = err.Error()
		s.r.logger().WarnContext(s.base, "stuck-queue scheduler", "err", err)
	case err == nil && s.warned != "":
		s.warned = ""
		s.r.logger().InfoContext(s.base, "stuck-queue scheduler error cleared")
	}
}
