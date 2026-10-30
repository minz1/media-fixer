package sweep

import (
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
)

func SummaryCardForTest(sr db.SweepRun) card.Card {
	return summaryCard(sr)
}
