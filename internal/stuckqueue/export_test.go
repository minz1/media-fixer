package stuckqueue

import "github.com/minz1/mediafixer/internal/card"

func StuckCardForTest(items []Item, capHit bool) card.Card {
	return stuckCard(items, capHit)
}
