package babysit_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/babysit"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func TestShowSeasonCounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(7, "Seven")
	future := now0().AddDate(0, 0, 5)
	fut := aired(4, 1, 4, false)
	fut.AirDateUtc = &future
	special := aired(9, 0, 1, false)
	e.son.episodes[7] = []client.Episode{
		aired(1, 1, 1, true), aired(2, 1, 2, false), aired(3, 1, 3, false), fut, special, aired(5, 2, 1, true),
	}
	e.son.queue = []client.QueueDetail{{SeriesID: 7, EpisodeID: 3}, {SeriesID: 8, EpisodeID: 2}}

	s, err := e.svc.Show(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []babysit.SeasonCounts{
		{Season: 1, Aired: 3, OnDisk: 1, Missing: 1, Queued: 1},
		{Season: 2, Aired: 1, OnDisk: 1},
	}
	if len(s.Seasons) != 2 || s.Seasons[0] != want[0] || s.Seasons[1] != want[1] {
		t.Fatalf("seasons = %+v", s.Seasons)
	}
	if s.Series == nil || s.Series.ID != 7 {
		t.Fatalf("series = %+v", s.Series)
	}
}

func TestShowLast50LiveItems(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(7, "Seven")
	live, dry := e.run(false), e.run(true)
	for i := range 60 {
		e.item(live, 7, "Seven", "rel"+strconv.Itoa(i), now0().Add(time.Duration(i)*time.Minute))
	}
	e.item(dry, 7, "Seven", "dryrel", now0().Add(time.Hour*24))

	s, err := e.svc.Show(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Items) != 50 || s.Items[0].ReleaseTitle != "rel59" || s.Items[49].ReleaseTitle != "rel10" {
		t.Fatalf("items = %d first=%q", len(s.Items), s.Items[0].ReleaseTitle)
	}
}

func TestShowSonarr404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.svc.Show(context.Background(), 99); !errors.Is(err, client.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestShowSonarr500KeepsDBParts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(7, "Seven")
	e.son.seriesStatus[7] = http.StatusInternalServerError
	e.target(7, "Seven", 3, 2)

	s, err := e.svc.Show(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if s.Series != nil || s.SonarrErr == "" || len(s.Targets) != 1 || !s.Stuck {
		t.Fatalf("show = %+v", s)
	}
}

func TestShowQueueKnown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(7, "Seven")
	e.has = false
	s, err := e.svc.Show(context.Background(), 7)
	if err != nil || s.QueueKnown {
		t.Fatalf("QueueKnown=%v err=%v", s.QueueKnown, err)
	}
	e.has = true
	e.state = stuckqueue.Result{Apps: []stuckqueue.AppResult{{
		App: stuckqueue.AppSonarr, Items: []stuckqueue.Item{queueItem(7, "q"), queueItem(8, "other")},
	}}}
	s, err = e.svc.Show(context.Background(), 7)
	if err != nil || !s.QueueKnown || len(s.Queue) != 1 || !s.Stuck {
		t.Fatalf("show = %+v err=%v", s, err)
	}
}

func TestShowHistoryLaziness(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(7, "Seven")
	e.target(7, "Seven", 0, 1)
	if _, err := e.svc.Show(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if n := e.son.hit("/api/v3/history/series"); n != 0 {
		t.Fatalf("history calls for non-stuck show = %d", n)
	}
	e.target(7, "Seven", 3, 1)
	if _, err := e.svc.Show(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if n := e.son.hit("/api/v3/history/series"); n != 1 {
		t.Fatalf("history calls for stuck show = %d", n)
	}
}
