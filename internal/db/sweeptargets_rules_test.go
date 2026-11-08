package db_test

import (
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func TestS4_Copies_StuckBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		streak, after int
		want          bool
	}{{2, 3, false}, {3, 3, true}, {4, 3, true}, {0, 1, false}, {1, 1, true}} {
		if got := (db.SweepTarget{Streak: tc.streak}).Stuck(tc.after); got != tc.want {
			t.Errorf("streak %d after %d: Stuck = %v, want %v", tc.streak, tc.after, got, tc.want)
		}
	}
}

func TestS4_Copies_EligibleBoundary(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	tg := db.SweepTarget{NextEligible: at}
	if !tg.Eligible(at) {
		t.Error("now == NextEligible must be eligible")
	}
	if tg.Eligible(at.Add(-time.Nanosecond)) {
		t.Error("one ns before NextEligible must not be eligible")
	}
	if !tg.Eligible(at.Add(time.Hour)) {
		t.Error("after NextEligible must be eligible")
	}
	var absent db.SweepTarget
	if !absent.Eligible(at) {
		t.Error("an absent target must be eligible")
	}
}
