package babysit_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/babysit"

	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/sweep"
)

func rowIDs(l babysit.List) []int {
	var ids []int
	for _, r := range l.Rows {
		ids = append(ids, r.SeriesID)
	}
	return ids
}

func TestListMembership(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for id, title := range map[int]string{1: "A", 2: "B", 3: "C", 4: "D", 5: "E", 6: "F"} {
		e.addSeries(id, title)
	}
	e.target(1, "A", 0, 2)
	e.item(e.run(false), 2, "B", "B.S01", now0())
	e.item(e.run(true), 3, "C", "C.S01", now0())
	e.state = stuckqueue.Result{Apps: []stuckqueue.AppResult{
		{
			App:     stuckqueue.AppSonarr,
			Items:   []stuckqueue.Item{queueItem(4, "d"), queueItem(0, "unmapped")},
			Waiting: []stuckqueue.Item{queueItem(5, "e")},
		},
	}}

	l, err := e.svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := rowIDs(l)
	slices.Sort(got)
	if !slices.Equal(got, []int{1, 2, 4}) {
		t.Fatalf("rows = %v, want [1 2 4]", got)
	}
}

func TestListOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for id, title := range map[int]string{1: "bravo", 2: "Alpha", 3: "charlie", 4: "Able"} {
		e.addSeries(id, title)
	}
	e.target(1, "bravo", 3, 2)
	e.target(3, "charlie", 0, 1)
	e.target(4, "Able", 1, 1)
	e.state = stuckqueue.Result{Apps: []stuckqueue.AppResult{{
		App: stuckqueue.AppSonarr, Items: []stuckqueue.Item{queueItem(2, "x")},
	}}}

	l, err := e.svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := rowIDs(l); !slices.Equal(got, []int{1, 2, 4, 3}) {
		t.Fatalf("order = %v, want [1 2 4 3]", got)
	}
	r := l.Rows[0]
	if r.Seasons != 1 || r.Missing != 2 || r.StuckSeasons != 1 || l.Rows[1].QueueItems != 1 {
		t.Fatalf("row = %+v", r)
	}
}

func TestListExcludedAndPickable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(1, "A")
	e.addSeries(2, "B")
	e.addSeries(3, "C")
	e.son.series[2].Monitored = false
	e.target(1, "A", 0, 1)
	e.target(2, "B", 0, 1)
	if err := sweep.SetExcluded(context.Background(), e.db, 2, true); err != nil {
		t.Fatal(err)
	}

	l, err := e.svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := rowIDs(l); !slices.Equal(got, []int{1}) {
		t.Fatalf("rows = %v", got)
	}
	if len(l.Excluded) != 1 || l.Excluded[0].ID != 2 {
		t.Fatalf("excluded = %+v", l.Excluded)
	}
	if len(l.Pickable) != 1 || l.Pickable[0].ID != 1 {
		t.Fatalf("pickable = %+v", l.Pickable)
	}
}

func TestListSonarrDownFallsBackToDB(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(1, "Live Title")
	e.son.listStatus = http.StatusInternalServerError
	e.target(1, "Stored Title", 0, 1)
	e.item(e.run(false), 2, "Item Title", "x", now0())

	l, err := e.svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.SonarrErr == "" || len(l.Pickable) != 0 {
		t.Fatalf("SonarrErr=%q pickable=%v", l.SonarrErr, l.Pickable)
	}
	titles := map[int]string{}
	for _, r := range l.Rows {
		titles[r.SeriesID] = r.Title
	}
	if titles[1] != "Stored Title" || titles[2] != "Item Title" {
		t.Fatalf("titles = %v", titles)
	}
}
