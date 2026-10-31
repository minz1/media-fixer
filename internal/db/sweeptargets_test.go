package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func TestDB_SweepTargets_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openTestDB(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	a := db.SweepTarget{
		Target: "s:1:2", SeriesID: 1, SeriesTitle: "A", Season: 2, Streak: 1, Missing: 5, Attempts: 2,
		LastAttempt: at, NextEligible: at.Add(48 * time.Hour),
	}
	b := db.SweepTarget{Target: "s:3:1", SeriesID: 3, Season: 1, Missing: 1, LastAttempt: at, NextEligible: at}
	for _, tg := range []db.SweepTarget{a, b} {
		if err := d.UpsertSweepTarget(ctx, tg); err != nil {
			t.Fatal(err)
		}
	}
	a.Streak, a.Missing, a.Attempts = 2, 4, 3
	a.LastAttempt = at.Add(time.Hour).In(time.FixedZone("X", 3600))
	if err := d.UpsertSweepTarget(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := d.ListSweepTargets(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, err %v", got, err)
	}
	g := got["s:1:2"]
	if g.Streak != 2 || g.Missing != 4 || g.Attempts != 3 || g.SeriesTitle != "A" || g.Season != 2 {
		t.Errorf("not overwritten: %+v", g)
	}
	if !g.LastAttempt.Equal(at.Add(time.Hour)) || g.LastAttempt.Location() != time.UTC {
		t.Errorf("LastAttempt = %v (%v)", g.LastAttempt, g.LastAttempt.Location())
	}
	if !g.NextEligible.Equal(at.Add(48*time.Hour)) || g.NextEligible.Location() != time.UTC {
		t.Errorf("NextEligible = %v", g.NextEligible)
	}
	if err = d.DeleteSweepTargets(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err = d.DeleteSweepTargets(ctx, []string{"s:1:2"}); err != nil {
		t.Fatal(err)
	}
	got, err = d.ListSweepTargets(ctx)
	if err != nil || len(got) != 1 || got["s:3:1"].SeriesID != 3 {
		t.Errorf("after delete: %+v, err %v", got, err)
	}
}

func TestDB_Migration19_Additive(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	rawExec(t, path, `DROP TABLE sweep_targets`, `DELETE FROM schema_version WHERE version = 19`)

	d2, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	got, err := d2.ListSweepTargets(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("table not recreated on a v18 db: %v, %v", got, err)
	}
	if n := rawQuery(t, path, `SELECT COUNT(*) FROM schema_version WHERE version = 19`); n != "1" {
		t.Errorf("version 19 rows = %s", n)
	}
}
