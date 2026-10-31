package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func TestBabysitExplanationRoundTrip(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	if _, err := d.GetBabysitExplanation(ctx, 7); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("empty get err = %v, want ErrNotFound", err)
	}
	e := db.BabysitExplanation{SeriesID: 7, Fingerprint: "f1", Text: "first", Model: "m", CreatedAt: sweepT0()}
	if err := d.PutBabysitExplanation(ctx, e); err != nil {
		t.Fatal(err)
	}
	e.Fingerprint, e.Text, e.CreatedAt = "f2", "second", sweepT0().Add(time.Hour)
	if err := d.PutBabysitExplanation(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetBabysitExplanation(ctx, 7)
	if err != nil || got != e {
		t.Fatalf("get = %+v, %v; want %+v", got, err, e)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
	}
}

func TestDB_Migration20_Additive(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if n := rawQuery(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'babysit_explanations'`); n != "1" {
		t.Fatalf("fresh db table count = %s", n)
	}
	rawExec(t, path, `DROP TABLE babysit_explanations`, `DELETE FROM schema_version WHERE version = 20`)

	d2, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err = d2.GetBabysitExplanation(context.Background(), 1); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("table not recreated on a v19 db: %v", err)
	}
	if n := rawQuery(t, path, `SELECT COUNT(*) FROM schema_version WHERE version = 20`); n != "1" {
		t.Errorf("version 20 rows = %s", n)
	}
}
