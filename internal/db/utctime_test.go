package db_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // raw connection, for asserting stored text

	"github.com/minz1/mediafixer/internal/db"
)

// modernc.org/sqlite stores a [time.Time] as RFC3339 text carrying the writing
// process's local offset, and SQLite compares DATETIME columns as strings — so
// two values with different offsets compare by their digits, not their
// instants. Every staleness sweep is such a comparison, which means a hung run
// or a due pending outcome can simply stop being found. These tests pin both
// halves of the fix: new writes are UTC, and existing rows were rewritten.

func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "utc.db")
}

// rawQuery reads a single value. Note every timestamp query below CASTs to
// TEXT: the driver parses a DATETIME column into a [time.Time] and, scanning
// that into a string, re-renders it as RFC3339Nano — so without the cast a
// test sees the driver's rendering rather than the bytes actually stored.
func rawQuery(t *testing.T, path, query string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var got string
	if err = raw.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, s := range stmts {
		if _, err = raw.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// TestTimestampsAreStoredAsUTC pins the write side: every DATETIME this
// package binds must carry a Z, never a local offset, or it cannot be
// text-compared against the others.
func TestTimestampsAreStoredAsUTC(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "tester",
		What: "cant_play", Title: "UTC Fixture",
	}
	if err = d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err = d.AddReporter(ctx, inc.ID, "alice", "discord", "d-alice"); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{MediaType: "tv", Title: "UTC Fixture", StartedAt: time.Now()}
	if err = d.SetPendingOutcome(ctx, inc.ID, po, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	d.Close()

	for _, q := range []string{
		"SELECT CAST(created_at AS TEXT) FROM incidents LIMIT 1",
		"SELECT CAST(updated_at AS TEXT) FROM incidents LIMIT 1",
		"SELECT CAST(pending_outcome_next_check AS TEXT) FROM incidents LIMIT 1",
		"SELECT CAST(reported_at AS TEXT) FROM incident_reporters LIMIT 1",
	} {
		got := rawQuery(t, path, q)
		if !strings.HasSuffix(got, "+00:00") {
			t.Errorf("%s stored %q; want a UTC value (+00:00), or it cannot be "+
				"text-compared against timestamps written at another offset", q, got)
		}
		if strings.Contains(got, "EDT") || strings.Contains(got, "EST") {
			t.Errorf("%s stored %q in Go's time.String() layout; SQLite cannot parse "+
				"that and it carries a local offset", q, got)
		}
	}
}

// TestUTCTimestampsMigration_RewritesOffsetBearingRows pins the migration. It
// simulates the real production state — rows written at a local offset,
// alongside Z-suffixed rows from the backfill's CURRENT_TIMESTAMP — by writing
// them raw and rolling schema_version back so the migration runs again.
func TestUTCTimestampsMigration_RewritesOffsetBearingRows(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusInvestigating, Source: "discord", ReportedBy: "tester",
		What: "cant_play", Title: "Legacy Row",
	}
	if err = d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Exactly what the old driver default wrote: Go's time.Time.String()
	// layout, at a local offset. 14:30 -0400 is 18:30 UTC.
	rawExec(t, path,
		`UPDATE incidents SET created_at = '2026-09-20 14:30:00 -0400 EDT',
		                      updated_at = '2026-09-20 14:30:00 -0400 EDT'`,
		`DELETE FROM schema_version WHERE version = 12`,
	)

	d2, err := db.Open(path) // migration 12 runs again
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	got := rawQuery(t, path, "SELECT CAST(updated_at AS TEXT) FROM incidents LIMIT 1")
	if !strings.HasSuffix(got, "+00:00") {
		t.Fatalf("updated_at = %q after migration; want a UTC value (+00:00)", got)
	}
	if !strings.HasPrefix(got, "2026-09-20 18:30:00") {
		t.Errorf("updated_at = %q; want the same instant expressed as 18:30 UTC — "+
			"the rewrite must preserve the value, not reinterpret it", got)
	}

	// The point of the rewrite: a genuinely stale row is now found by a
	// threshold bound as a normal Go time. Before it, the digits of a
	// -04:00 value could sort as newer than a UTC threshold two hours later.
	stale, err := d2.FindStaleInvestigating(ctx, time.Date(2026, 9, 20, 19, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Errorf("FindStaleInvestigating returned %d rows, want 1: a row last touched at "+
			"18:30 UTC is stale against a 19:00 UTC threshold", len(stale))
	}
}

// TestUTCTimestampsMigration_LeavesUnparseableValuesAlone guards the IS NOT
// NULL condition: strftime returns NULL for anything it cannot parse, so
// without it the migration would clobber such a row instead of skipping it.
func TestUTCTimestampsMigration_LeavesUnparseableValuesAlone(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "t",
		What: "cant_play", Title: "Odd Row",
	}
	if err = d.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	d.Close()

	rawExec(t, path,
		`UPDATE incidents SET updated_at = 'not a timestamp'`,
		`DELETE FROM schema_version WHERE version = 12`,
	)

	d2, err := db.Open(path)
	if err != nil {
		t.Fatalf("Open failed on a row with an unparseable timestamp: %v", err)
	}
	defer d2.Close()

	if got := rawQuery(t, path, "SELECT CAST(updated_at AS TEXT) FROM incidents LIMIT 1"); got != "not a timestamp" {
		t.Errorf("updated_at = %q; an unparseable value must be left alone, not overwritten", got)
	}
}

func TestMain(m *testing.M) {
	// Run this package's tests in a non-UTC zone, so a regression that lets a
	// local offset reach the database is observable here rather than only on
	// the deployed host. Set before any test runs, via the environment rather
	// than by reassigning time.Local.
	if os.Getenv("TZ") == "" {
		os.Setenv("TZ", "America/New_York")
	}
	os.Exit(m.Run())
}
