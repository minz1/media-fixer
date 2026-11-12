package stuckqueue

import (
	"context"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

func StuckCardForTest(items []Item, capHit bool) card.Card {
	c, _ := stuckCard(items, capHit)
	return c
}

func SetOpStoreForTest(s *Scheduler, store opexec.Store) { s.store = store }

func ApplyForTest(ctx context.Context, r *Runner, app App, events map[Key]Event) ([]Snapshot, []Effect, error) {
	return r.apply(ctx, app, events)
}

func ToRowForTest(s Snapshot) db.StuckRow { return toRow(s) }

func FromRowForTest(r db.StuckRow) Snapshot { return fromRow(r) }

type (
	SlotEvent = slotEvent
	SlotKind  = slotKind
)

const (
	HolderNone         = holderNone
	HolderRun          = holderRun
	HolderOwner        = holderOwner
	HolderHousekeeping = holderHousekeeping
	SlotClaim          = slotClaim
	SlotRelease        = slotRelease
)

func SlotNext(cur Holder, ev SlotEvent) (Holder, error) { return nextSlot(cur, ev) }

func Refusal(holder, claimant Holder) error { return refusal(holder, claimant) }

func SlotHolders() []Holder {
	return []Holder{holderNone, holderRun, holderOwner, holderHousekeeping}
}

func SlotKinds() []SlotKind { return []SlotKind{slotClaim, slotRelease} }

func SlotEventOf(k SlotKind, h Holder) SlotEvent { return slotEvent{Kind: k, Holder: h} }

func ClaimForTest(s *Scheduler, h Holder, now time.Time) (func(), error) {
	release, err := s.claim(h, now)
	if err != nil {
		return nil, err
	}
	return func() { release(nil) }, nil
}

var (
	ErrNotHeld     = errNotHeld
	ErrWrongHolder = errWrongHolder
)

func RegrabSpecForTest(d Download) operation.Spec { return regrabSpec(d) }

func TransitionIDsForTest() []string {
	var ids []string
	for _, r := range transitions() {
		ids = append(ids, r.id)
	}
	return ids
}
