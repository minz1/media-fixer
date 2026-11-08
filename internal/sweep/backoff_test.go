package sweep_test

import (
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func TestNextEligible(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := sweep.Settings{BackoffBaseHours: 24, BackoffMaxDays: 30}
	day := 24 * time.Hour
	for _, tc := range []struct {
		streak int
		want   time.Duration
	}{{0, day}, {1, 2 * day}, {4, 16 * day}, {5, 30 * day}, {200, 30 * day}, {1 << 30, 30 * day}} {
		if got := sweep.NextEligibleForTest(last, tc.streak, s); !got.Equal(last.Add(tc.want)) {
			t.Errorf("streak %d: got %v, want +%v", tc.streak, got.Sub(last), tc.want)
		}
	}
}

func TestAttemptTarget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := sweep.Settings{BackoffBaseHours: 24, BackoffMaxDays: 30}
	ser := client.Series{ID: 7, Title: "Show"}
	prev := &db.SweepTarget{Streak: 2, Missing: 5, Attempts: 3}
	for _, tc := range []struct {
		name                   string
		prev                   *db.SweepTarget
		missing, streak, tries int
	}{
		{"first", nil, 5, 0, 1},
		{"no drop", prev, 6, 3, 4},
		{"equal", prev, 5, 3, 4},
		{"drop", prev, 4, 0, 4},
	} {
		got := sweep.AttemptTargetForTest(tc.prev, "s:7:1", ser, 1, tc.missing, now, s)
		if got.Streak != tc.streak || got.Attempts != tc.tries || got.Missing != tc.missing ||
			got.Target != "s:7:1" || got.SeriesID != 7 || got.SeriesTitle != "Show" || got.Season != 1 ||
			!got.LastAttempt.Equal(now) || !got.NextEligible.Equal(sweep.NextEligibleForTest(now, tc.streak, s)) {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}

func TestS4_Copies_StuckTargetsBoundary(t *testing.T) {
	t.Parallel()
	rows := map[string]db.SweepTarget{
		"s:1:1": {Target: "s:1:1", Streak: 2},
		"s:1:2": {Target: "s:1:2", Streak: 3},
	}
	got := sweep.StuckTargets(rows, 3)
	if len(got) != 1 || got[0].Target != "s:1:2" {
		t.Fatalf("StuckTargets(after 3) = %+v, want only the streak-3 row", got)
	}
	for _, r := range rows {
		if r.Stuck(3) != (r.Target == got[0].Target) {
			t.Errorf("%s: StuckTargets and SweepTarget.Stuck disagree", r.Target)
		}
	}
}
