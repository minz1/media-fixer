package stuckqueue

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/opexec"
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

	mu    sync.Mutex
	wg    sync.WaitGroup
	slot  slot
	store opexec.Store
}

func NewScheduler(base context.Context, r *Runner, d *db.DB) *Scheduler {
	return &Scheduler{r: r, d: d, base: base, store: d, slot: slot{holder: holderNone}}
}

func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) Start() {
	s.wg.Go(func() {
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
	})
}

func (s *Scheduler) Tick(now time.Time) {
	set, err := LoadSettings(s.base, s.d)
	s.warnOnce(err)
	s.housekeep(now, set.ImportTimeout())
	if err != nil || !set.Enabled {
		return
	}
	s.mu.Lock()
	due := s.slot.lastStart.IsZero() || now.Sub(s.slot.lastStart) >= set.Interval()
	s.mu.Unlock()
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

func (s *Scheduler) State() (Result, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slot.last, s.slot.hasLast, s.slot.holder == holderRun
}

func (s *Scheduler) StartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slot.lastStart
}

func (s *Scheduler) start(now time.Time, set Settings) error {
	release, err := s.claim(holderRun, now)
	if err != nil {
		return err
	}
	s.wg.Go(func() {
		res := s.r.Run(s.base, set)
		release(func(sl *slot) {
			sl.last, sl.hasLast = res, true
		})
		s.refresh(cardPrefix)
	})
	return nil
}

func (s *Scheduler) warnOnce(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err != nil && err.Error() != s.slot.warned:
		s.slot.warned = err.Error()
		s.r.logger().WarnContext(s.base, "stuck-queue scheduler", "err", err)
	case err == nil && s.slot.warned != "":
		s.slot.warned = ""
		s.r.logger().InfoContext(s.base, "stuck-queue scheduler error cleared")
	}
}
