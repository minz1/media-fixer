package sweep_test

import (
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func failure(id, series, season int, dl, event string, at time.Time) client.HistoryRecord {
	return client.HistoryRecord{
		ID: id, SeriesID: series, DownloadID: dl, EventType: event, Date: at,
		Episode: &client.HistoryEpisode{SeasonNumber: season},
	}
}

func TestBursts(t *testing.T) {
	t.Parallel()
	now := t0()
	recent := now.Add(-time.Hour)
	distinct := func(n int, at time.Time) []client.HistoryRecord {
		var out []client.HistoryRecord
		for i := range n {
			out = append(out, failure(i+1, 1, 2, string(rune('A'+i)), "downloadFailed", at))
		}
		return out
	}
	packWith := func(others int) []client.HistoryRecord {
		var out []client.HistoryRecord
		for i := range 26 {
			out = append(out, failure(i+1, 1, 2, "PACK", "downloadFailed", recent))
		}
		for i := range others {
			out = append(out, failure(100+i, 1, 2, string(rune('A'+i)), "downloadFailed", recent))
		}
		return out
	}
	noEpisode := distinct(5, recent)
	for i := range noEpisode {
		noEpisode[i].Episode = nil
	}
	otherEvent := distinct(5, recent)
	for i := range otherEvent {
		otherEvent[i].EventType = "grabbed"
	}
	emptyID := distinct(5, recent)
	for i := range emptyID {
		emptyID[i].DownloadID = ""
	}
	attempted := map[string]db.SweepTarget{"s:1:2": {LastAttempt: now.Add(-30 * time.Minute)}}

	tests := []struct {
		name    string
		recs    []client.HistoryRecord
		targets map[string]db.SweepTarget
		want    bool
	}{
		{"five downloads", distinct(5, recent), nil, true},
		{"four downloads", distinct(4, recent), nil, false},
		{"one pack on 26 records plus three", packWith(3), nil, false},
		{"one pack on 26 records plus four", packWith(4), nil, true},
		{"empty download id counts each record", emptyID, nil, true},
		{"before the last attempt", distinct(5, recent), attempted, false},
		{"older than seven days", distinct(5, now.Add(-8*24*time.Hour)), nil, false},
		{"nil episode", noEpisode, nil, false},
		{"other event type", otherEvent, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sweep.Bursts(tc.recs, tc.targets, now, 5)
			if got["s:1:2"] != tc.want || len(got) > 1 {
				t.Errorf("bursts = %v, want s:1:2 = %v", got, tc.want)
			}
		})
	}
}
