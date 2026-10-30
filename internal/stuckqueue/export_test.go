package stuckqueue

import "github.com/minz1/mediafixer/internal/card"

func StuckCardForTest(items []Item, capHit bool) card.Card {
	return stuckCard(items, capHit)
}

func SetLastForTest(s *Scheduler, res Result, running bool) {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.last, s.hasLast, s.running = res, true, running
}
