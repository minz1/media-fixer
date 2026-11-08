package stuckqueue

import (
	"context"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/opexec"
)

func StuckCardForTest(items []Item, capHit bool) card.Card {
	c, _ := stuckCard(items, capHit)
	return c
}

func SetLastForTest(s *Scheduler, res Result, running bool) {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.last, s.hasLast, s.running = res, true, running
}

func SetOpStoreForTest(s *Scheduler, store opexec.Store) { s.store = store }

func ApplyForTest(ctx context.Context, r *Runner, app App, events map[Key]Event) ([]Snapshot, []Effect, error) {
	return r.apply(ctx, app, events)
}

func ToRowForTest(s Snapshot) db.StuckRow { return toRow(s) }

func FromRowForTest(r db.StuckRow) Snapshot { return fromRow(r) }
