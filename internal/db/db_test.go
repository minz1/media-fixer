package db_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	d, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestCreateAndGetIncident(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusOpen,
		Source:     "discord",
		ReportedBy: "alice",
		What:       "cant_play",
		Title:      "Breaking Bad",
		Details:    "won't load",
	}
	lifecycletest.Incident(t, d, inc)
	if inc.ID == "" {
		t.Fatal("expected ID to be set")
	}

	got, err := d.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != inc.Title {
		t.Errorf("title: got %q want %q", got.Title, inc.Title)
	}
	if got.Status != db.StatusOpen {
		t.Errorf("status: got %q want %q", got.Status, db.StatusOpen)
	}
}

func TestFindOpenByTitle_Dedup(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusOpen,
		Source:     "seerr",
		ReportedBy: "bob",
		What:       "cant_play",
		Title:      "The Wire",
	}
	lifecycletest.Incident(t, d, inc)

	found, err := d.FindOpenByTitle(ctx, "The Wire")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != inc.ID {
		t.Errorf("got %q want %q", found.ID, inc.ID)
	}

	// Resolved incidents should not match.
	lifecycletest.MoveTo(t, d, inc.ID, db.StatusResolved)
	_, err = d.FindOpenByTitle(ctx, "The Wire")
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("expected ErrNotFound for resolved incident, got %v", err)
	}
}

const testIncidentCount = 3

func TestCountOpenIncidents(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	for i := range testIncidentCount {
		lifecycletest.Incident(t, d, &db.Incident{
			Status:     db.StatusOpen,
			Source:     "discord",
			ReportedBy: "user",
			What:       "cant_play",
			Title:      string(rune('A' + i)),
		})
	}

	n, err := d.CountOpenIncidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != testIncidentCount {
		t.Errorf("count: got %d want %d", n, testIncidentCount)
	}
}

func TestLogAndListActions(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "x", What: "cant_play", Title: "T",
	}
	lifecycletest.Incident(t, d, inc)

	a := &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "refresh_links",
		Params:      map[string]any{"torrent": "foo"},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}
	if err := d.LogAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" {
		t.Fatal("expected ID")
	}

	now := time.Now()
	actions, err := d.ListActions(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions want 1", len(actions))
	}
	got := actions[0]
	if got.Action != "refresh_links" {
		t.Errorf("action: %q", got.Action)
	}
	if got.AppliedAt == nil || got.AppliedAt.Before(now.Add(-time.Second)) {
		t.Error("applied_at not set correctly")
	}
}

const testReporterCount = 2

func TestReporters(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "a", What: "cant_play", Title: "T",
	}
	lifecycletest.Incident(t, d, inc)

	base, err := d.ListReporters(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"alice", "bob", "alice"} { // alice twice → dedup
		if err = d.AddReporter(ctx, inc.ID, r, "discord", ""); err != nil {
			t.Fatal(err)
		}
	}

	reporters, err := d.ListReporters(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) != len(base)+testReporterCount {
		t.Errorf("reporters: got %d want %d more than %d", len(reporters), testReporterCount, len(base))
	}
}

// TestReporters_DedupeSameDiscordUserDifferentDisplayName reproduces the
// duplicate-DM bug: the same Discord user reports under two different display
// names (a nickname change between calls, or a retried /report interaction).
// The partial unique index on (incident_id, discord_user_id) must reject the
// second row at write time, so a single person is stored — and therefore
// notified — exactly once, regardless of which reader is used.
func TestReporters_DedupeSameDiscordUserDifferentDisplayName(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "fixture", What: "cant_play", Title: "T",
	}
	lifecycletest.Incident(t, d, inc)

	if err := d.AddReporter(ctx, inc.ID, "alice", "discord", "discord-user-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddReporter(ctx, inc.ID, "alice-new-nick", "discord", "discord-user-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddReporter(ctx, inc.ID, "bob", "discord", "discord-user-2"); err != nil {
		t.Fatal(err)
	}

	// Structural guarantee: the second nick for discord-user-1 never became a row.
	reporters, err := d.ListReporters(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) != 3 {
		t.Errorf("reporter rows: got %d want 3 (fixture + alice + bob) — %v", len(reporters), reporters)
	}

	ids, err := d.ListDiscordReporterIDs(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids: got %d want 2 (one per unique discord user) — %v", len(ids), ids)
	}
}

func TestSettings(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	paused, err := d.IsAutonomousPaused(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if paused {
		t.Error("should not be paused by default")
	}

	if setErr := d.SetSetting(ctx, "autonomous_paused", "true"); setErr != nil {
		t.Fatal(setErr)
	}
	paused, err = d.IsAutonomousPaused(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Error("should be paused after setting")
	}
}

// TestPendingOutcome_SetGetClear covers the basic CRUD lifecycle: not found
// before anything is set, round-trips through JSON correctly.
func TestPendingOutcome_SetGetClear(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Rick and Morty",
	}
	lifecycletest.Incident(t, d, inc)

	if _, err := d.GetPendingOutcome(ctx, inc.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before anything is set, got %v", err)
	}

	po := &db.PendingOutcome{
		MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9,
		StartedAt: time.Now(), LastStage: "searching",
	}
	lifecycletest.Track(t, d, inc.ID, po, time.Now().Add(5*time.Minute))
	got, err := d.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Rick and Morty" || got.Season != 9 || got.Episode != 9 || got.LastStage != "searching" {
		t.Fatalf("got %+v", got)
	}

	lifecycletest.MoveTo(t, d, inc.ID, db.StatusResolved)
	if _, getErr := d.GetPendingOutcome(ctx, inc.ID); !errors.Is(getErr, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after clearing, got %v", getErr)
	}
}

// last_disruption (RecordDisruption/LastDisruption) no longer exists as a
// dedicated table/method pair — LastDisruption is now a query journal.Journal
// answers over incident_events. See internal/journal/journal_test.go for its
// equivalent coverage (no rows before anything is recorded, and the most
// recent disruptive action_applied event always wins).

func TestOpen_EnablesWALAndForeignKeys(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	mode, err := d.JournalModeForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode: got %q, want %q (Open's DSN previously used mattn/go-sqlite3 "+
			"syntax that modernc.org/sqlite silently ignores)", mode, "wal")
	}

	fkOn, err := d.ForeignKeysEnabledForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !fkOn {
		t.Error("foreign_keys: got disabled, want enabled")
	}
}

func TestOpen_ForeignKeyCascadeDeletesChildren(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "X"}
	lifecycletest.Incident(t, d, inc)
	if err := d.AddReporter(ctx, inc.ID, "alice", "discord", "123"); err != nil {
		t.Fatal(err)
	}
	action := &db.ActionLog{
		IncidentID: inc.ID, Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied,
	}
	if err := d.LogAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	const insertEvent = `INSERT INTO incident_events (incident_id, at, kind, payload, source)
		VALUES (?, ?, 'llm_round', '{}', 'live')`
	if err := d.ExecForTest(ctx, insertEvent, inc.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	// No exported DeleteIncident exists (the app never deletes incidents), so
	// exercise the cascade directly against the write pool.
	if err := d.ExecForTest(ctx, `DELETE FROM incidents WHERE id = ?`, inc.ID); err != nil {
		t.Fatalf("delete incident: %v", err)
	}

	if actions, err := d.ListActions(ctx, inc.ID); err != nil || len(actions) != 0 {
		t.Errorf("actions_log: got %d rows, err %v; want cascade-deleted", len(actions), err)
	}
	if reporters, err := d.ListReporters(ctx, inc.ID); err != nil || len(reporters) != 0 {
		t.Errorf("incident_reporters: got %d rows, err %v; want cascade-deleted", len(reporters), err)
	}
	if events, err := d.EventsSince(ctx, inc.ID, 0); err != nil || len(events) != 0 {
		t.Errorf("incident_events: got %d rows, err %v; want cascade-deleted", len(events), err)
	}
}

func TestOpen_Idempotent(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/reopen.db"

	first, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	firstVersions, err := first.SchemaVersionsForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := first.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	second, err := db.Open(path)
	if err != nil {
		t.Fatalf("reopening an already-migrated database should not error: %v", err)
	}
	t.Cleanup(func() { second.Close() })

	secondVersions, err := second.SchemaVersionsForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(firstVersions) != len(secondVersions) {
		t.Fatalf("schema_version grew on reopen: first %v, second %v", firstVersions, secondVersions)
	}

	// And the database still works normally afterward.
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "bob", What: "cant_play", Title: "Y"}
	lifecycletest.Incident(t, second, inc)
}

func TestOpen_BootstrapsLegacyDatabase(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/legacy.db"

	// Build a fixture that looks exactly like what the pre-schema_version
	// Open() produced: apply the base schema, then the four ALTER TABLE
	// migrations, then dedup + the unique index — all unconditionally, with
	// no schema_version bookkeeping at all. This is the real shape of the
	// production database as it exists today.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string) {
		t.Helper()
		if _, execErr := raw.Exec(q); execErr != nil {
			t.Fatalf("legacy fixture setup: %v", execErr)
		}
	}
	// discord_user_id is already part of the current incident_reporters
	// CREATE TABLE (it was folded into the base schema at some point after
	// the ALTER-based migration that originally added it), so only the three
	// incidents.* columns still genuinely rely on the ALTER path here.
	mustExec(db.LegacySchemaForTest)
	mustExec(`ALTER TABLE incidents ADD COLUMN last_heartbeat DATETIME`)
	mustExec(`ALTER TABLE incidents ADD COLUMN pending_outcome TEXT`)
	mustExec(`ALTER TABLE incidents ADD COLUMN pending_outcome_next_check DATETIME`)
	mustExec(db.LegacyDedupReportersByDiscordIDForTest)
	mustExec(db.LegacyCreateReporterDiscordIndexForTest)
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("bootstrap against a legacy database should not error: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	versions, err := d.SchemaVersionsForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 1-6 are the reconciled legacy migrations this fixture simulates; 7-13
	// are the new ones, which run unconditionally on top of any database
	// (legacy or fresh) since they postdate schema_version.
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}
	if len(versions) != len(want) {
		t.Fatalf("schema_version after bootstrap: got %v, want %v", versions, want)
	}
	for i, v := range want {
		if versions[i] != v {
			t.Fatalf("schema_version after bootstrap: got %v, want %v", versions, want)
		}
	}

	// And the database is fully usable afterward.
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "carol", What: "cant_play", Title: "Z"}
	lifecycletest.Incident(t, d, inc)
}

func TestOpen_DetectsExistingForeignKeyViolations(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/legacy-orphan.db"

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := raw.Exec(db.LegacySchemaForTest); execErr != nil {
		t.Fatal(execErr)
	}
	// Foreign keys were never enforced by the legacy Open() (see the dsn
	// mismatch this whole migration exists to fix), so an orphaned child row
	// like this could genuinely exist in a long-running production database.
	const insertOrphan = `INSERT INTO actions_log (id, incident_id, action, triggered_by, status)
		VALUES ('a1', 'does-not-exist', 'x', 'agent', 'applied')`
	if _, execErr := raw.Exec(insertOrphan); execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	_, err = db.Open(path)
	if err == nil {
		t.Fatal("expected Open to fail loudly on a pre-existing foreign key violation, got nil error")
	}
	if !strings.Contains(err.Error(), "foreign_key_check") {
		t.Errorf("error should mention foreign_key_check, got: %v", err)
	}
}

func TestOpen_ReadPoolNotBlockedByWriteTransaction(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	txn, err := d.BeginWriteTxForTest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Rollback()
	// SQLite doesn't actually take the write lock until the first write
	// statement in the transaction runs.
	const touchSetting = `UPDATE settings SET value = value WHERE key = 'autonomous_paused'`
	if _, execErr := txn.ExecContext(ctx, touchSetting); execErr != nil {
		t.Fatal(execErr)
	}

	done := make(chan error, 1)
	go func() { done <- d.PingReadForTest(ctx) }()

	select {
	case readErr := <-done:
		if readErr != nil {
			t.Fatalf("read pool query failed while a write transaction was open "+
				"(WAL not actually enabled?): %v", readErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read pool query did not complete while a write transaction was open — " +
			"reads are blocking on writes, defeating the point of splitting the pools")
	}
}

func TestSeerrIssues(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "seerr", ReportedBy: "alice", What: "cant_play", Title: "Andor"}
	lifecycletest.Incident(t, d, inc)
	for _, id := range []string{"12", "13", "12"} {
		if _, err := d.AddSeerrIssue(ctx, inc.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.ListSeerrIssueIDs(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "12,13" {
		t.Errorf("issue ids = %v, want [12 13]", got)
	}
}

func TestSeerrIssuesMigration_CreatesTableOnExistingDB(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	rawExec(t, path, `DROP TABLE incident_seerr_issues`, `DELETE FROM schema_version WHERE version = 14`)

	d2, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err = d2.ListSeerrIssueIDs(context.Background(), "missing"); err != nil {
		t.Errorf("table not recreated on an existing db: %v", err)
	}
}

func TestMigration15_AddsLastReporterNoteAt(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	got := rawQuery(t, path, `SELECT COUNT(*) FROM pragma_table_info('incidents') WHERE name = 'last_reporter_note_at'`)
	if got != "1" {
		t.Errorf("last_reporter_note_at columns = %s, want 1", got)
	}
}

func newClaimIncident(t *testing.T, d *db.DB) string {
	t.Helper()
	inc := &db.Incident{Status: db.StatusOpen, Source: "seerr", ReportedBy: "alice", What: "cant_play", Title: "Andor"}
	lifecycletest.Incident(t, d, inc)
	return inc.ID
}

func TestClaimReporterNote_Gap(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id := newClaimIncident(t, d)
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	for _, c := range []struct {
		at   time.Time
		gap  time.Duration
		want bool
	}{
		{t0, 30 * time.Minute, true},
		{t0.Add(10 * time.Minute), 30 * time.Minute, false},
		{t0.Add(31 * time.Minute), 30 * time.Minute, true},
		{t0.Add(31 * time.Minute), 0, true},
	} {
		got, err := d.ClaimReporterNote(ctx, id, c.at, c.gap)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("claim at %s gap %s = %v, want %v", c.at.Sub(t0), c.gap, got, c.want)
		}
	}
}

func TestClaimReporterNote_Race(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id := newClaimIncident(t, d)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	results := make(chan bool, 10)
	errs := make(chan error, 10)
	for range 10 {
		go func() {
			ok, err := d.ClaimReporterNote(ctx, id, now, 30*time.Minute)
			errs <- err
			results <- ok
		}()
	}
	wins := 0
	for range 10 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("claims won = %d, want 1", wins)
	}
}

func TestAddSeerrIssue_ReportsInsert(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id := newClaimIncident(t, d)

	for i, want := range []bool{true, false} {
		got, err := d.AddSeerrIssue(ctx, id, "12")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("call %d inserted = %v, want %v", i+1, got, want)
		}
	}
}

func TestListActions_DistinctAppliedAt(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T"}
	lifecycletest.Incident(t, d, inc)
	first := time.Now().Add(-time.Hour).UTC()
	for _, at := range []time.Time{first, time.Now().UTC()} {
		if err := d.LogAction(ctx, &db.ActionLog{
			IncidentID:  inc.ID,
			Action:      "restart_jellyfin",
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
			AppliedAt:   &at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	actions, err := d.ListActions(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || !actions[0].AppliedAt.Equal(first) {
		t.Errorf("first applied_at = %v, want %v", actions[0].AppliedAt, first)
	}
}

func TestFindUnresolvedByTitle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	create := func(d *db.DB, status db.IncidentStatus, title string) *db.Incident {
		t.Helper()
		inc := &db.Incident{
			Status: status, Source: "media-agent", ReportedBy: "media-agent", What: "stuck_read", Title: title,
		}
		lifecycletest.Incident(t, d, inc)
		return inc
	}

	for _, st := range []db.IncidentStatus{
		db.StatusOpen, db.StatusInvestigating, db.StatusVerifying, db.StatusReopened,
		db.StatusManualTestNeeded, db.StatusBlocked, db.StatusAgentFixed,
	} {
		t.Run(string(st), func(t *testing.T) {
			t.Parallel()
			d := openTestDB(t)
			inc := create(d, st, "The Wire (2002)")
			got, err := d.FindUnresolvedByTitle(ctx, "the wire")
			if err != nil || got.ID != inc.ID {
				t.Errorf("got %v, %v want %s", got, err, inc.ID)
			}
		})
	}

	t.Run("resolved", func(t *testing.T) {
		t.Parallel()
		d := openTestDB(t)
		create(d, db.StatusResolved, "The Wire")
		if _, err := d.FindUnresolvedByTitle(ctx, "The Wire"); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("got %v", err)
		}
	})

	t.Run("resolved and unresolved", func(t *testing.T) {
		t.Parallel()
		d := openTestDB(t)
		create(d, db.StatusResolved, "The Wire")
		want := create(d, db.StatusAgentFixed, "The Wire")
		got, err := d.FindUnresolvedByTitle(ctx, "The Wire")
		if err != nil || got.ID != want.ID {
			t.Errorf("got %v, %v want %s", got, err, want.ID)
		}
	})

	t.Run("wildcards are literal", func(t *testing.T) {
		t.Parallel()
		d := openTestDB(t)
		create(d, db.StatusOpen, "Fifty Off")
		for _, q := range []string{"%", "Fifty_Off", "F%"} {
			if _, err := d.FindUnresolvedByTitle(ctx, q); !errors.Is(err, db.ErrNotFound) {
				t.Errorf("%q: got %v", q, err)
			}
		}
	})
}
