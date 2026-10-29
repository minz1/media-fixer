package db_test

import (
	"context"
	"testing"
	"time"
)

func TestUpsertStuckSeen_KeepsFirstSeen(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	first, err := d.UpsertStuckSeen(ctx, "sonarr", "D", "Old title", "gap", t0)
	if err != nil {
		t.Fatal(err)
	}
	if !first.FirstSeenAt.Equal(t0) || first.NotifiedAt != nil {
		t.Fatalf("first = %+v", first)
	}
	again, err := d.UpsertStuckSeen(ctx, "sonarr", "D", "New title", "clear", t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.FirstSeenAt.Equal(t0) || again.Title != "New title" || again.Verdict != "clear" {
		t.Fatalf("again = %+v", again)
	}
}

func TestMarkStuckNotified(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	if _, err := d.UpsertStuckSeen(ctx, "sonarr", "D", "T", "gap", t0); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkStuckNotified(ctx, "sonarr", "D", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := d.UpsertStuckSeen(ctx, "sonarr", "D", "T", "gap", t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.NotifiedAt == nil || !got.NotifiedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("notified = %v", got.NotifiedAt)
	}
}

func TestPruneStuckGone(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, r := range [][2]string{{"sonarr", "A"}, {"sonarr", "B"}, {"radarr", "A"}} {
		if _, err := d.UpsertStuckSeen(ctx, r[0], r[1], "T", "gap", t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.PruneStuckGone(ctx, "sonarr", []string{"A"}); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(time.Hour)
	b, _ := d.UpsertStuckSeen(ctx, "sonarr", "B", "T", "gap", later)
	a, _ := d.UpsertStuckSeen(ctx, "sonarr", "A", "T", "gap", later)
	ra, _ := d.UpsertStuckSeen(ctx, "radarr", "A", "T", "gap", later)
	if !b.FirstSeenAt.Equal(later) || !a.FirstSeenAt.Equal(t0) || !ra.FirstSeenAt.Equal(t0) {
		t.Fatalf("after prune: a=%v b=%v radarr-a=%v", a.FirstSeenAt, b.FirstSeenAt, ra.FirstSeenAt)
	}
	if err := d.PruneStuckGone(ctx, "radarr", nil); err != nil {
		t.Fatal(err)
	}
	if ra2, _ := d.UpsertStuckSeen(
		ctx,
		"radarr",
		"A",
		"T",
		"gap",
		later.Add(time.Hour),
	); !ra2.FirstSeenAt.Equal(
		later.Add(time.Hour),
	) {
		t.Fatalf("empty keep did not prune radarr: %v", ra2.FirstSeenAt)
	}
}
