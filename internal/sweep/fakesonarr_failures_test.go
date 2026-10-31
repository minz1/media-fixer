package sweep_test

import (
	"context"
	"testing"
	"time"
)

func TestFakeSonarr_FailuresSince(t *testing.T) {
	t.Parallel()
	f := newFakeSonarr(t, &fakeClock{t: t0()})
	added := t0().Add(-48 * time.Hour)
	f.addSeriesAdded(1, "Show", added)
	f.addFailure(1, 7, 2, "DL1", t0())
	f.addFailure(1, 8, 3, "DL2", t0().Add(-time.Hour))

	series, err := f.client().ListSeries(context.Background())
	if err != nil || len(series) != 1 || !series[0].Added.Equal(added) {
		t.Fatalf("series = %+v, err %v", series, err)
	}
	recs, err := f.client().FailuresSince(context.Background(), t0().Add(-30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].DownloadID != "DL1" || recs[0].Episode == nil || recs[0].Episode.SeasonNumber != 2 {
		t.Errorf("failures = %+v", recs)
	}
	all, err := f.client().HistorySince(context.Background(), t0().Add(-2*time.Hour))
	if err != nil || len(all) != 2 || all[0].Episode != nil {
		t.Errorf("history = %+v, err %v", all, err)
	}
}
