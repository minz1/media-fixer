package stuckqueue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/card"
)

type Holder string

const (
	holderNone         Holder = "none"
	holderRun          Holder = "run"
	holderOwner        Holder = "owner"
	holderHousekeeping Holder = "housekeeping"
)

type slotKind string

const (
	slotClaim   slotKind = "claim"
	slotRelease slotKind = "release"
)

type slotEvent struct {
	Kind   slotKind
	Holder Holder
}

type refusedError string

func (e refusedError) Error() string { return string(e) }

func (e refusedError) Is(target error) bool { return target == ErrRefused }

const (
	ErrOwnerActing    refusedError = "an owner action is running, try again in a moment"
	errRunActiveOwner refusedError = "a stuck-queue run is active, try again in a moment"
	errHousekeeping   refusedError = "a background check is running, try again in a moment"
)

var (
	errNotHeld     = errors.New("not_held")
	errWrongHolder = errors.New("wrong_holder")
)

type update func(*slot)

type slot struct {
	holder    Holder
	lastStart time.Time
	last      Result
	hasLast   bool
	warned    string
}

func nextSlot(cur Holder, ev slotEvent) (Holder, error) {
	switch ev.Kind {
	case slotClaim:
		if cur == holderNone {
			return ev.Holder, nil
		}
		return cur, refusal(cur, ev.Holder)
	case slotRelease:
		switch {
		case cur == holderNone:
			return cur, errNotHeld
		case cur != ev.Holder:
			return cur, errWrongHolder
		}
		return holderNone, nil
	}
	return cur, errWrongHolder
}

func refusal(holder, claimant Holder) error {
	switch holder {
	case holderRun:
		if claimant == holderRun {
			return ErrRunActive
		}
		return errRunActiveOwner
	case holderOwner:
		return ErrOwnerActing
	case holderHousekeeping:
		return errHousekeeping
	case holderNone:
	}
	return nil
}

func (s *Scheduler) claim(c Holder, now time.Time) (func(update), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base.Err() != nil {
		return nil, card.ErrRestarting
	}
	next, err := nextSlot(s.slot.holder, slotEvent{Kind: slotClaim, Holder: c})
	if err != nil {
		return nil, err
	}
	s.slot.holder = next
	if c == holderRun {
		s.slot.lastStart = now
	}
	var once sync.Once
	return func(u update) { once.Do(func() { s.release(c, u) }) }, nil
}

func (s *Scheduler) release(c Holder, u update) {
	s.mu.Lock()
	var last []byte
	if u != nil {
		u(&s.slot)
		if s.slot.hasLast {
			last, _ = json.Marshal(s.slot.last)
		}
	}
	next, err := nextSlot(s.slot.holder, slotEvent{Kind: slotRelease, Holder: c})
	if err == nil {
		s.slot.holder = next
	}
	s.mu.Unlock()
	if err != nil {
		s.r.logger().ErrorContext(s.base, "stuck-queue: slot release rejected", "holder", c, "err", err)
	}
	if last != nil {
		if err := s.d.SetSetting(context.WithoutCancel(s.base), lastResultKey, string(last)); err != nil {
			s.r.logger().WarnContext(s.base, "stuck-queue: store last result", "err", err)
		}
	}
}
