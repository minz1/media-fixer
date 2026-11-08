package sweep

import (
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/card"
)

type slotState string

const (
	slotIdle    slotState = "idle"
	slotLive    slotState = "live"
	slotDry     slotState = "dry"
	slotStopped slotState = "stopped"
)

type slotKind string

const (
	slotStart      slotKind = "start"
	slotDone       slotKind = "done"
	slotCancelLive slotKind = "cancel_live"
	slotReconcile  slotKind = "reconcile"
	slotStop       slotKind = "stop"
)

type effKind string

const (
	effLaunch     effKind = "launch"
	effCancel     effKind = "cancel"
	effCrashStale effKind = "crash_stale"
)

type slotEff struct {
	Kind  effKind
	Cause error
}

type slotSnap struct {
	State   slotState
	Trigger string
	Dry     bool
	Since   time.Time
}

type slotEvent struct {
	Kind    slotKind
	Trigger string
	Dry     bool
	Enabled bool
	Paused  bool
	At      time.Time
	Cause   error
}

var (
	errSlotNoRun   = errors.New("no_run")
	errSlotBusy    = errors.New("run_active")
	errSlotStopped = errors.New("already_stopped")
)

func nextSlot(cur slotSnap, ev slotEvent) (slotSnap, []slotEff, error) {
	switch cur.State {
	case slotIdle:
		return nextIdle(cur, ev)
	case slotLive, slotDry:
		return nextRunning(cur, ev)
	case slotStopped:
		return nextStopped(cur, ev)
	}
	return cur, nil, fmt.Errorf("unknown slot state %q", cur.State)
}

func nextIdle(cur slotSnap, ev slotEvent) (slotSnap, []slotEff, error) {
	switch ev.Kind {
	case slotStart:
		switch {
		case !ev.Dry && !ev.Enabled:
			return cur, nil, ErrDisabled
		case ev.Paused:
			return cur, nil, ErrPaused
		}
		state := slotLive
		if ev.Dry {
			state = slotDry
		}
		return slotSnap{State: state, Trigger: ev.Trigger, Dry: ev.Dry, Since: ev.At}, []slotEff{{Kind: effLaunch}}, nil
	case slotDone:
		return cur, nil, errSlotNoRun
	case slotCancelLive:
		return cur, nil, nil
	case slotReconcile:
		return cur, []slotEff{{Kind: effCrashStale}}, nil
	case slotStop:
		return slotSnap{State: slotStopped}, nil, nil
	}
	return cur, nil, fmt.Errorf("unknown slot event %q", ev.Kind)
}

func nextRunning(cur slotSnap, ev slotEvent) (slotSnap, []slotEff, error) {
	switch ev.Kind {
	case slotStart:
		if !ev.Dry && !ev.Enabled {
			return cur, nil, ErrDisabled
		}
		return cur, nil, ErrRunActive
	case slotDone:
		return slotSnap{State: slotIdle}, nil, nil
	case slotCancelLive:
		if cur.State == slotLive {
			return cur, []slotEff{{Kind: effCancel, Cause: ev.Cause}}, nil
		}
		return cur, nil, nil
	case slotReconcile:
		return cur, nil, errSlotBusy
	case slotStop:
		return slotSnap{State: slotStopped}, nil, nil
	}
	return cur, nil, fmt.Errorf("unknown slot event %q", ev.Kind)
}

func nextStopped(cur slotSnap, ev slotEvent) (slotSnap, []slotEff, error) {
	switch ev.Kind {
	case slotStart, slotCancelLive, slotReconcile:
		return cur, nil, card.ErrRestarting
	case slotDone:
		return cur, nil, nil
	case slotStop:
		return cur, nil, errSlotStopped
	}
	return cur, nil, fmt.Errorf("unknown slot event %q", ev.Kind)
}

func checkSlot(s slotSnap) error {
	switch s.State {
	case slotIdle, slotStopped:
		if s.Trigger != "" || !s.Since.IsZero() || s.Dry {
			return fmt.Errorf("%s slot carries a trigger, a start time or the dry flag: %+v", s.State, s)
		}
	case slotLive, slotDry:
		switch {
		case s.Trigger == "":
			return fmt.Errorf("%s slot has no trigger", s.State)
		case s.Since.IsZero():
			return fmt.Errorf("%s slot has no start time", s.State)
		case s.Dry != (s.State == slotDry):
			return fmt.Errorf("%s slot has dry=%v", s.State, s.Dry)
		}
	default:
		return fmt.Errorf("unknown slot state %q", s.State)
	}
	return nil
}
