package sweep

import (
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
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

func NextEligibleForTest(last time.Time, streak int, s Settings) time.Time {
	return nextEligible(last, streak, s)
}

func AttemptTargetForTest(
	prev *db.SweepTarget, key string, series client.Series, season, missing int, now time.Time, s Settings,
) db.SweepTarget {
	return attemptTarget(prev, key, series, season, missing, now, s)
}

func EligibleForTest(t db.SweepTarget, ok bool, now time.Time) bool { return eligible(t, ok, now) }

func DigestCardForTest(stuck []db.SweepTarget) card.Card { return digestCard(stuck) }
