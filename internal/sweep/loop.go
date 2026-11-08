package sweep

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

var errNotStarted = errors.New("the sweep scheduler is not started")

type askKind int

const (
	askTick askKind = iota
	askRunNow
	askRunSeries
	askCancelLive
	askStatus
)

type ask struct {
	kind    askKind
	at      time.Time
	trigger string
	dry     bool
	series  client.Series
	pass    Pass
	cause   error
	reply   chan answer
}

type answer struct {
	err       error
	cancelled bool
	view      SlotView
}

type SlotView struct {
	Active  bool
	Trigger string
	Dry     bool
	Since   time.Time
}

type runEnd struct {
	trigger string
	err     error
}

type digestEnd struct {
	err error
	bad string
}

type owned struct {
	slot       slotSnap
	cancel     context.CancelCauseFunc
	run        <-chan runEnd
	digest     <-chan digestEnd
	tickWarn   string
	digestWarn string
	badDigest  string
}

type startReq struct {
	trigger string
	dry     bool
	set     Settings
	sc      scope
	at      time.Time
}

func (s *Scheduler) call(ctx context.Context, q ask) (answer, error) {
	if s.base.Err() != nil {
		return answer{}, card.ErrRestarting
	}
	if !s.started.Load() {
		return answer{}, errNotStarted
	}
	q.reply = make(chan answer, 1)
	select {
	case s.inbox <- q:
	case <-s.base.Done():
		return answer{}, card.ErrRestarting
	case <-ctx.Done():
		return answer{}, ctx.Err()
	}
	select {
	case a := <-q.reply:
		return a, nil
	case <-ctx.Done():
		return answer{}, ctx.Err()
	}
}

func (s *Scheduler) loop(ctx context.Context, ticks <-chan time.Time) {
	o := owned{slot: slotSnap{State: slotIdle}}
	for {
		select {
		case <-ctx.Done():
			s.stop(&o)
			return
		case now := <-ticks:
			s.reap(&o)
			_ = s.onTick(ctx, &o, now)
		case q := <-s.inbox:
			s.reap(&o)
			q.reply <- s.handle(ctx, &o, q)
		case e := <-o.run:
			s.endRun(&o, e)
		case e := <-o.digest:
			s.endDigest(&o, e)
		}
	}
}

func (s *Scheduler) stop(o *owned) {
	o.slot, _, _ = nextSlot(o.slot, slotEvent{Kind: slotStop})
}

func (s *Scheduler) reap(o *owned) {
	for {
		select {
		case e := <-o.run:
			s.endRun(o, e)
		case e := <-o.digest:
			s.endDigest(o, e)
		default:
			return
		}
	}
}

func (s *Scheduler) endRun(o *owned, e runEnd) {
	o.slot, _, _ = nextSlot(o.slot, slotEvent{Kind: slotDone})
	o.run, o.cancel = nil, nil
	if e.err != nil {
		s.r.logger().ErrorContext(s.base, "pack sweep run", "trigger", e.trigger, "err", e.err)
	}
}

func (s *Scheduler) endDigest(o *owned, e digestEnd) {
	o.digest = nil
	o.badDigest = e.bad
	s.latch(&o.digestWarn, dropQuiet(e.err))
}

func dropQuiet(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (s *Scheduler) latch(warned *string, err error) {
	switch {
	case err != nil && err.Error() != *warned:
		*warned = err.Error()
		s.r.logger().WarnContext(s.base, "pack sweep scheduler", "err", err)
	case err == nil && *warned != "":
		*warned = ""
		s.r.logger().InfoContext(s.base, "pack sweep scheduler error cleared")
	}
}

func (s *Scheduler) handle(ctx context.Context, o *owned, q ask) answer {
	if ctx.Err() != nil {
		return answer{err: card.ErrRestarting}
	}
	switch q.kind {
	case askTick:
		return answer{err: s.onTick(ctx, o, q.at)}
	case askRunNow:
		return answer{err: s.runNow(o, q.trigger, q.dry)}
	case askRunSeries:
		return answer{err: s.runSeries(o, q.series, q.pass)}
	case askCancelLive:
		return s.cancelLive(o, q.cause)
	case askStatus:
		return answer{view: viewOf(o.slot)}
	}
	return answer{err: errors.New("unknown sweep request")}
}

func viewOf(sn slotSnap) SlotView {
	return SlotView{
		Active:  sn.State == slotLive || sn.State == slotDry,
		Trigger: sn.Trigger, Dry: sn.Dry, Since: sn.Since,
	}
}

func (s *Scheduler) feed(o *owned, ev slotEvent) ([]slotEff, error) {
	next, effs, err := nextSlot(o.slot, ev)
	o.slot = next
	return effs, err
}

func (s *Scheduler) cancelLive(o *owned, cause error) answer {
	effs, err := s.feed(o, slotEvent{Kind: slotCancelLive, Cause: cause})
	if err != nil {
		return answer{err: err}
	}
	var a answer
	for _, e := range effs {
		if e.Kind == effCancel && o.cancel != nil {
			o.cancel(e.Cause)
			a.cancelled = true
		}
	}
	return a
}

func (s *Scheduler) onTick(ctx context.Context, o *owned, now time.Time) error {
	if ctx.Err() != nil {
		return card.ErrRestarting
	}
	set, serr := LoadSettings(s.base, s.d)
	rerr := s.reconcile(o, now)
	var err error
	if serr == nil && set.Enabled {
		s.startDigest(o, set, now)
		err = s.runDue(o, set, now)
		if errors.Is(err, ErrRunActive) || errors.Is(err, ErrPaused) {
			err = nil
		}
	}
	err = dropQuiet(errors.Join(rerr, serr, err))
	s.latch(&o.tickWarn, err)
	return err
}

func (s *Scheduler) reconcile(o *owned, now time.Time) error {
	ids, err := s.d.RunningSweepIDs(s.base)
	if err != nil || len(ids) == 0 {
		return err
	}
	effs, ferr := s.feed(o, slotEvent{Kind: slotReconcile, At: now})
	crash := ferr == nil && slices.ContainsFunc(effs, func(e slotEff) bool { return e.Kind == effCrashStale })
	if !crash {
		return nil
	}
	crashed, err := s.d.CrashRunningSweeps(s.base)
	if s.r.Refresh != nil {
		for _, id := range crashed {
			s.r.Refresh(card.SweepSubject(id))
		}
	}
	return err
}

func (s *Scheduler) startDigest(o *owned, set Settings, now time.Time) {
	if o.digest != nil {
		return
	}
	end := make(chan digestEnd, 1)
	o.digest = end
	bad := o.badDigest
	s.jobs.Go(func() {
		err := s.digest(set, now, &bad)
		end <- digestEnd{err: err, bad: bad}
	})
}

func (s *Scheduler) runDue(o *owned, set Settings, now time.Time) error {
	next, err := s.next(set, now)
	if err != nil || now.Before(next) {
		return err
	}
	u, err := s.Used(now)
	if err != nil {
		return err
	}
	return s.startRun(o, startReq{trigger: triggerSchedule, set: Remaining(set, u), at: now})
}

func (s *Scheduler) runNow(o *owned, trigger string, dry bool) error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil {
		return err
	}
	if !dry && !set.Enabled {
		return ErrDisabled
	}
	now := s.r.Now()
	if !dry {
		u, uerr := s.Used(now)
		if uerr != nil {
			return uerr
		}
		set = Remaining(set, u)
	}
	return s.startRun(o, startReq{trigger: trigger, dry: dry, set: set, at: now})
}

func (s *Scheduler) runSeries(o *owned, series client.Series, pass Pass) error {
	set, err := LoadSettings(s.base, s.d)
	if err != nil {
		return err
	}
	switch {
	case !set.Enabled:
		return ErrDisabled
	case slices.Contains(set.ExcludedSeries, series.ID):
		return ErrExcluded
	case !series.Monitored:
		return ErrUnmonitored
	case series.SeriesType == seriesAnime && !set.Anime:
		return ErrAnimeOff
	}
	now := s.r.Now()
	u, err := s.Used(now)
	if err != nil {
		return err
	}
	set = Remaining(set, u)
	if pass == PassPacks {
		set.MaxSearches = 0
		if set.MaxLookups == 0 || set.MaxGrabs == 0 {
			return ErrNoBudget
		}
	} else {
		set.MaxGrabs, set.MaxLookups = 0, 0
		if set.MaxSearches == 0 {
			return ErrNoBudget
		}
	}
	return s.startRun(o, startReq{
		trigger: db.BabysitTrigger + " " + series.Title, set: set,
		sc: scope{series: &series, pass: pass}, at: now,
	})
}

func (s *Scheduler) startRun(o *owned, q startReq) error {
	paused := false
	if o.slot.State == slotIdle {
		var err error
		if paused, err = s.d.IsAutonomousPaused(s.base); err != nil {
			return err
		}
	}
	effs, err := s.feed(o, slotEvent{
		Kind: slotStart, Trigger: q.trigger, Dry: q.dry, Enabled: q.set.Enabled, Paused: paused, At: q.at,
	})
	if err != nil {
		return err
	}
	for _, e := range effs {
		if e.Kind == effLaunch {
			s.launch(o, q)
		}
	}
	return nil
}

func (s *Scheduler) launch(o *owned, q startReq) {
	ctx, cancel := context.WithCancelCause(s.base)
	end := make(chan runEnd, 1)
	o.cancel, o.run = cancel, end
	s.jobs.Go(func() {
		_, err := s.r.runScoped(ctx, q.trigger, q.dry, q.set, q.sc)
		cancel(nil)
		end <- runEnd{trigger: q.trigger, err: err}
	})
}
