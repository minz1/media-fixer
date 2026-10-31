package sweep

import (
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const day = 24 * time.Hour

func nextEligible(last time.Time, streak int, s Settings) time.Time {
	limit := time.Duration(s.BackoffMaxDays) * day
	wait := time.Duration(s.BackoffBaseHours) * time.Hour
	for i := 0; i < streak && wait < limit; i++ {
		wait *= 2
	}
	return last.Add(min(wait, limit))
}

func attemptTarget(
	prev *db.SweepTarget, key string, series client.Series, season, missing int, now time.Time, s Settings,
) db.SweepTarget {
	streak, attempts := 0, 1
	if prev != nil {
		attempts = prev.Attempts + 1
		if missing >= prev.Missing {
			streak = prev.Streak + 1
		}
	}
	return db.SweepTarget{
		Target: key, SeriesID: series.ID, SeriesTitle: series.Title, Season: season, Streak: streak,
		Missing: missing, Attempts: attempts, LastAttempt: now, NextEligible: nextEligible(now, streak, s),
	}
}

func eligible(t db.SweepTarget, ok bool, now time.Time) bool {
	return !ok || !now.Before(t.NextEligible)
}
