package stuckqueue

import (
	"github.com/minz1/mediafixer/internal/card"
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
