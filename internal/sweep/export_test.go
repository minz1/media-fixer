package sweep

import (
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
)

func SummaryCardForTest(sr db.SweepRun) card.Card {
	return summaryCard(sr)
}

func SetProgressForTest(r *Runner, p *Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = p
}

func CountsTextForTest(c db.SweepCounts, dry bool) string { return countsText(c, dry) }

func StartCardForTest(id int64, trigger string, dry bool, s Settings) card.Card {
	return startCard(id, trigger, dry, s)
}
