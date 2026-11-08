package db_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

const oldBinaryRefusedInsert = `INSERT INTO sweep_refused (title_key, release_title, indexer, refused_at, reason) VALUES (?, ?, ?, ?, ?)`

func insertOldRefused(t *testing.T, d *db.DB, key, reason string, at time.Time) {
	t.Helper()
	if err := d.ExecForTest(context.Background(), oldBinaryRefusedInsert, key, key, "Comet", at, reason); err != nil {
		t.Fatal(err)
	}
}

func refusedCodes(t *testing.T, d *db.DB, since time.Time) map[string]sweepstate.RefusalCode {
	t.Helper()
	list, err := d.ListSweepRefused(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]sweepstate.RefusalCode{}
	for _, r := range list {
		out[r.TitleKey] = r.Code
	}
	return out
}

func TestMigration25_BackfillFromReason(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	cases := []struct {
		key, reason string
		want        sweepstate.RefusalCode
	}{
		{"torrent", "500 x code=torrent_blocked", sweepstate.TorrentBlocked},
		{"provider", "502 x code=provider_error", sweepstate.ProviderError},
		{"both", "500 provider_error then torrent_blocked", sweepstate.TorrentBlocked},
		{"plain", `500 {"message":"x"}`, sweepstate.OtherRefusal},
		{"timeout", "Post https://x/release: context deadline exceeded", sweepstate.OtherRefusal},
		{"empty", "", sweepstate.OtherRefusal},
	}
	for _, c := range cases {
		insertOldRefused(t, d, c.key, c.reason, sweepT0())
	}
	if err := d.BackfillRefusalCodesForTest(ctx); err != nil {
		t.Fatal(err)
	}
	got := refusedCodes(t, d, sweepT0().Add(-time.Hour))
	for _, c := range cases {
		if got[c.key] != c.want {
			t.Errorf("%s: code = %q, want %q", c.key, got[c.key], c.want)
		}
	}
	if n := rawCount(t, d, `SELECT COUNT(*) FROM sweep_refused WHERE code IS NULL`); n != 0 {
		t.Errorf("rows with NULL code after the backfill = %d", n)
	}
}

func rawCount(t *testing.T, d *db.DB, query string) int {
	t.Helper()
	n, err := d.QueryIntForTest(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigration25_OldBinaryInsertAccepted(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertOldRefused(t, d, "old", "500 x code=torrent_blocked", sweepT0())
	insertOldRefused(t, d, "older", "gateway timeout", sweepT0().Add(-time.Minute))
	if n := rawCount(t, d, `SELECT COUNT(*) FROM sweep_refused WHERE code IS NULL`); n != 2 {
		t.Fatalf("NULL code rows = %d, want 2", n)
	}
	got := refusedCodes(t, d, sweepT0().Add(-time.Hour))
	if got["old"] != sweepstate.TorrentBlocked || got["older"] != sweepstate.OtherRefusal {
		t.Errorf("derived codes = %v", got)
	}
	if err := d.PruneSweepRefused(ctx, sweepT0()); err != nil {
		t.Fatal(err)
	}
	if n := rawCount(t, d, `SELECT COUNT(*) FROM sweep_refused WHERE code IS NULL`); n != 0 {
		t.Errorf("NULL code rows after prune = %d, want 0", n)
	}
	if n := rawCount(t, d, `SELECT COUNT(*) FROM sweep_refused WHERE code = 'torrent_blocked'`); n != 1 {
		t.Errorf("torrent_blocked rows after prune = %d, want 1", n)
	}
}

func TestMigration25_CheckRejectsUnknownCode(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertOldRefused(t, d, "a", "x", sweepT0())
	if err := d.ExecForTest(ctx, `UPDATE sweep_refused SET code = 'x'`); err == nil {
		t.Error("UPDATE with an unknown code succeeded")
	}
	if err := d.ExecForTest(ctx, `UPDATE sweep_refused SET code = 'other'`); err != nil {
		t.Errorf("UPDATE with a known code: %v", err)
	}
}

func TestMigration25_CodeSetMatchesSweepstate(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	ddl := rawQuery(t, path, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'sweep_refused'`)
	m := regexp.MustCompile(`code IN \(([^)]*)\)`).FindStringSubmatch(ddl)
	if m == nil {
		t.Fatalf("no code CHECK in %s", ddl)
	}
	var got []string
	for lit := range strings.SplitSeq(m[1], ",") {
		got = append(got, strings.Trim(strings.TrimSpace(lit), "'"))
	}
	var want []string
	for _, c := range sweepstate.RefusalCodes() {
		want = append(want, string(c))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("CHECK set = %v, want %v", got, want)
	}
}

func TestMigration25_CaseMatchesReadPath(t *testing.T) {
	t.Parallel()
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if got, want := norm(db.MigrationRefusalCaseForTest), norm(db.RefusalCodeSQLForTest); got != want {
		t.Errorf("migration CASE = %q, read path = %q", got, want)
	}
}

func TestS5_7_ExpiredRefusalsPruned(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	now := sweepT0()
	day := 24 * time.Hour
	rows := []struct {
		key, reason string
		at          time.Time
	}{
		{"torrent-in", "x code=torrent_blocked", now.Add(-90*day + time.Minute)},
		{"torrent-out", "x code=torrent_blocked", now.Add(-90*day - time.Minute)},
		{"other-in", "x", now.Add(-7*day + time.Minute)},
		{"other-out", "x", now.Add(-7*day - time.Minute)},
		{"provider-now", "x code=provider_error", now},
		{"provider-past", "x code=provider_error", now.Add(-time.Minute)},
	}
	for _, r := range rows {
		insertOldRefused(t, d, r.key, r.reason, r.at)
	}
	if err := d.AddSweepRefused(ctx, &db.SweepRefused{
		TitleKey:     "typed-out",
		ReleaseTitle: "t",
		Indexer:      "Comet",
		RefusedAt: now.Add(
			-7*day - time.Minute,
		),
		Reason: "x code=torrent_blocked",
		Code:   sweepstate.OtherRefusal,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.PruneSweepRefused(ctx, now); err != nil {
		t.Fatal(err)
	}
	got := refusedCodes(t, d, now.Add(-365*day))
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"other-in", "provider-now", "torrent-in"}; !slices.Equal(keys, want) {
		t.Errorf("kept = %v, want %v", keys, want)
	}
	if n := rawCount(t, d, `SELECT COUNT(*) FROM sweep_refused WHERE code IS NULL`); n != 0 {
		t.Errorf("NULL code rows after prune = %d", n)
	}
}
