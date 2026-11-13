package db_test

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func outboxRow(key string, at time.Time) db.OutboxRow {
	return db.OutboxRow{Key: key, Target: "owner", Kind: "card", Body: "{}", CreatedAt: at, NextAt: at}
}

func dueKeys(t *testing.T, d *db.DB, now time.Time) []string {
	t.Helper()
	rows, err := d.DueOutbox(context.Background(), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range rows {
		keys = append(keys, r.Key)
	}
	return keys
}

func TestF9_OutboxKeyIsIdempotent(t *testing.T) {
	t.Parallel()
	d := openDB(t, filepath.Join(t.TempDir(), "o.db"))
	ctx := context.Background()
	for range 2 {
		if err := d.EnqueueOutbox(ctx, outboxRow("inc:a:g1:v2:0", t20())); err != nil {
			t.Fatal(err)
		}
	}
	if got := dueKeys(t, d, t20()); !slices.Equal(got, []string{"inc:a:g1:v2:0"}) {
		t.Fatalf("due %v, want one row", got)
	}
}

func TestF9_DueOutboxInIDOrder(t *testing.T) {
	t.Parallel()
	d := openDB(t, filepath.Join(t.TempDir(), "o.db"))
	ctx := context.Background()
	for _, k := range []string{"c", "a", "b", "sent", "gone", "later"} {
		r := outboxRow(k, t20())
		if k == "later" {
			r.NextAt = t20().Add(time.Hour)
		}
		if err := d.EnqueueOutbox(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	all, err := d.DueOutbox(ctx, t20().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, r := range all {
		ids[r.Key] = r.ID
	}
	if err = d.MarkOutboxSent(ctx, ids["sent"], t20()); err != nil {
		t.Fatal(err)
	}
	if err = d.GiveUpOutbox(ctx, ids["gone"], t20()); err != nil {
		t.Fatal(err)
	}
	if got := dueKeys(t, d, t20()); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("due %v, want c a b (id order, sent/given up/not yet due left out)", got)
	}
	if err = d.MarkOutboxFailed(ctx, ids["a"], t20().Add(time.Minute), "discord down"); err != nil {
		t.Fatal(err)
	}
	got, err := d.DueOutbox(ctx, t20().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].Key != "a" || got[1].Attempts != 1 {
		t.Fatalf("after a failure: %+v, want a second with 1 attempt", got)
	}
	if n, cerr := d.StuckOutboxCount(ctx, t20().Add(time.Hour)); cerr != nil || n != 1 {
		t.Fatalf("StuckOutboxCount = %d, %v; want 1", n, cerr)
	}
	if n, cerr := d.StuckOutboxCount(ctx, t20().Add(8*24*time.Hour)); cerr != nil || n != 0 {
		t.Fatalf("StuckOutboxCount 8 days on = %d, %v; want 0", n, cerr)
	}
}

func TestF9_PruneOutboxDropsFinishedRowsAfter30Days(t *testing.T) {
	t.Parallel()
	d := openDB(t, filepath.Join(t.TempDir(), "o.db"))
	ctx := context.Background()
	for _, k := range []string{"old-sent", "old-gone", "pending", "new-sent"} {
		if err := d.EnqueueOutbox(ctx, outboxRow(k, t20())); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := d.DueOutbox(ctx, t20(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r.Key {
		case "old-sent":
			err = d.MarkOutboxSent(ctx, r.ID, t20())
		case "old-gone":
			err = d.GiveUpOutbox(ctx, r.ID, t20())
		case "new-sent":
			err = d.MarkOutboxSent(ctx, r.ID, t20().Add(20*24*time.Hour))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.PruneOutbox(ctx, t20().Add(31*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneOutbox = %d, %v; want 2", n, err)
	}
	for _, k := range []string{"old-sent", "old-gone", "pending", "new-sent"} {
		if err = d.EnqueueOutbox(ctx, outboxRow(k, t20())); err != nil {
			t.Fatal(err)
		}
	}
	if got := dueKeys(t, d, t20()); !slices.Equal(got, []string{"pending", "old-sent", "old-gone"}) {
		t.Fatalf("due after prune and re-enqueue %v, want pending kept and only the pruned keys free again", got)
	}
}

func TestMigration26_UpDownUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	if err := db.MigrateUpToForTest(fresh, 25); err != nil {
		t.Fatal(err)
	}
	want := master(t, rawOpen(t, fresh))

	path := filepath.Join(t.TempDir(), "o.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.EnqueueOutbox(ctx, outboxRow("k", t20())); err != nil {
		t.Fatal(err)
	}
	d.Close()

	strip27(t, path)
	var out bytes.Buffer
	if err = db.MigrateDown(ctx, path, 26, &out); err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, path)
	if got := master(t, raw); !slices.Equal(got, want) {
		t.Fatalf("sqlite_master after down differs from a fresh v25:\n%v\n%v", got, want)
	}
	if v := maxVersion(t, raw); v != 25 {
		t.Fatalf("version %d after down", v)
	}
	if !bytes.Contains(out.Bytes(), []byte("outbox rows dropped: 1")) {
		t.Fatalf("down output %q", out.String())
	}
	raw.Close()

	d = openDB(t, path)
	if got := dueKeys(t, d, t20()); len(got) != 0 {
		t.Fatalf("re-up kept rows %v", got)
	}
	if err = d.EnqueueOutbox(ctx, outboxRow("k2", t20())); err != nil {
		t.Fatalf("enqueue after re-up: %v", err)
	}
}
