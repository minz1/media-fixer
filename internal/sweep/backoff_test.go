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

func TestEligible(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	tg := db.SweepTarget{NextEligible: now}
	if !sweep.EligibleForTest(tg, false, now.Add(-time.Hour)) {
		t.Error("no row must be eligible")
	}
	if !sweep.EligibleForTest(tg, true, now) {
		t.Error("due row must be eligible")
	}
	if sweep.EligibleForTest(tg, true, now.Add(-time.Second)) {
		t.Error("early row must not be eligible")
	}
}
