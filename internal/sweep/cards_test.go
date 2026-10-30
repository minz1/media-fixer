package sweep_test

import (
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func TestSummaryCard(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	end := start.Add(83 * time.Second)
	counts := db.SweepCounts{Grabs: 1, MaxGrabs: 10, Lookups: 2, MaxLookups: 40, Searched: 3, MaxSearches: 200}
	cases := []struct {
		name string
		sr   db.SweepRun
		tone card.Tone
		want string
	}{
		{"done", db.SweepRun{Status: "done", StartedAt: start, FinishedAt: &end, Counts: counts}, card.Success,
			"Pack sweep done:\ngrabs 1/10, lookups 2/40, searched 3/200, imported 0, failed 0, refused 0, " +
				"still downloading 0, took 1m23s. /media/maintenance"},
		{
			"dry refused",
			db.SweepRun{Dry: true, Status: "refused", Reason: "why", StartedAt: start, FinishedAt: &end},
			card.Warn,
			"Pack sweep dry run refused (why):\ngrabs 0/0, lookups 0/0, searched 0/0, imported 0, failed 0, " +
				"refused 0, still downloading 0, took 1m23s. /media/maintenance",
		},
		{"unfinished", db.SweepRun{Status: "failed", StartedAt: start}, card.Warn,
			"Pack sweep failed:\ngrabs 0/0, lookups 0/0, searched 0/0, imported 0, failed 0, refused 0, " +
				"still downloading 0. /media/maintenance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := sweep.SummaryCardForTest(tc.sr)
			if c.Plain() != tc.want || c.Tone != tc.tone || c.Path != "/maintenance" {
				t.Errorf("card = %+v\nplain %q\nwant  %q", c, c.Plain(), tc.want)
			}
		})
	}
}
