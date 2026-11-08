package sweep

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	triggerSchedule = "schedule"
	maxRetries      = 2
)

var (
	ErrRunActive = errors.New("a run is already active")
	ErrDisabled  = errors.New("the sweep is disabled; only a Dry run can start")

	ErrExcluded    = errors.New("this show is excluded from the sweep; include it first")
	ErrUnmonitored = errors.New("this show is not monitored in Sonarr")
	ErrAnimeOff    = errors.New("this show is anime and the sweep's anime setting is off")
	ErrNoBudget    = errors.New("nothing is left of today's budget for this search")
)

type Scheduler struct {
	r    *Runner
	d    *db.DB
	base context.Context

	inbox   chan ask
	loopWG  sync.WaitGroup
	jobs    sync.WaitGroup
	started atomic.Bool
}

func NewScheduler(base context.Context, r *Runner, d *db.DB) *Scheduler {
	return &Scheduler{r: r, d: d, base: base, inbox: make(chan ask)}
}

func (s *Scheduler) Runner() *Runner { return s.r }

func (s *Scheduler) Wait() {
	s.loopWG.Wait()
	s.jobs.Wait()
}

func (s *Scheduler) Start() {
	ticker := time.NewTicker(time.Minute)
	s.started.Store(true)
	s.loopWG.Go(func() {
		defer ticker.Stop()
		s.loop(s.base, ticker.C)
	})
}

func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	a, err := s.call(ctx, ask{kind: askTick, at: now})
	return errors.Join(err, a.err)
}

func (s *Scheduler) RunNow(ctx context.Context, trigger string, dry bool) error {
	a, err := s.call(ctx, ask{kind: askRunNow, trigger: trigger, dry: dry})
	return errors.Join(err, a.err)
}

func (s *Scheduler) RunSeries(ctx context.Context, series client.Series, pass Pass) error {
	a, err := s.call(ctx, ask{kind: askRunSeries, series: series, pass: pass})
	return errors.Join(err, a.err)
}

func (s *Scheduler) CancelLive(ctx context.Context, cause error) (bool, error) {
	a, err := s.call(ctx, ask{kind: askCancelLive, cause: cause})
	return a.cancelled, errors.Join(err, a.err)
}

func (s *Scheduler) Status(ctx context.Context) (SlotView, error) {
	a, err := s.call(ctx, ask{kind: askStatus})
	return a.view, errors.Join(err, a.err)
}

func (s *Scheduler) Save(ctx context.Context, set Settings, now time.Time) error {
	if err := SaveSettings(ctx, s.d, set, now); err != nil {
		return err
	}
	if set.Enabled {
		return nil
	}
	if _, err := s.CancelLive(context.WithoutCancel(ctx), ErrDisabled); err != nil && !errors.Is(err, card.ErrRestarting) {
		return err
	}
	return nil
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
		set.MaxLookups, set.MaxSearches = 0, 0
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
	last, err := s.d.LastSweepRunStart(s.base)
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
