package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
)

func addIncident(t *testing.T, d *db.DB, id string) {
	t.Helper()
	err := d.ExecForTest(context.Background(), `INSERT INTO incidents (id, source, reported_by, what, title)
		VALUES (?, 'test', 'test', 'cant_play', 'T')`, id)
	if err != nil {
		t.Fatal(err)
	}
}

func seededOps(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ops.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"inc-fail", "inc-nores", "inc-run", "inc-done"} {
		addIncident(t, d, id)
	}
	fail := newOp(t, d, replaceSpecFor("inc-fail"))
	step(t, d, fail.ID, startEv(0), finishEv(0, operation.ResultDone), startEv(1), finishEv(1, operation.ResultFailed))
	nores := newOp(t, d, replaceSpecFor("inc-nores"))
	step(t, d, nores.ID, startEv(0), finishEv(0, operation.ResultFailed))
	newOp(t, d, replaceSpecFor("inc-run"))
	done := newOp(t, d, replaceSpecFor("inc-done"))
	finishDone(t, d, done.ID)
	dl := newOp(t, d, regrabSpecFor("dl-fail"))
	step(t, d, dl.ID, startEv(0), finishEv(0, operation.ResultDone), startEv(1), finishEv(1, operation.ResultFailed))
	d.Close()
	strip23(t, path)
	return path
}

func down22(t *testing.T, path string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := db.MigrateDown(context.Background(), path, 22, &out)
	return out.String(), err
}

func maxVersion(t *testing.T, raw *sql.DB) int {
	t.Helper()
	var v int
	err := raw.QueryRowContext(context.Background(), `SELECT MAX(version) FROM schema_version`).Scan(&v)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func masterOf(t *testing.T, path string) []string {
	t.Helper()
	raw := rawOpen(t, path)
	defer raw.Close()
	return master(t, raw)
}

func opCount(t *testing.T, raw *sql.DB) int {
	t.Helper()
	var n int
	err := raw.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM operations`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func clearedCount(t *testing.T, raw *sql.DB) int {
	t.Helper()
	var n int
	err := raw.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM incident_events WHERE kind = 'migration_cleared'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateDown22_DropsTableIndexesAndVersion(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	if err := db.MigrateUpToForTest(fresh, 21); err != nil {
		t.Fatal(err)
	}
	want := master(t, rawOpen(t, fresh))
	if _, err := down22(t, path); err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, path)
	if got := master(t, raw); !slices.Equal(got, want) {
		t.Fatalf("sqlite_master after down differs from a fresh v21:\n%v\n%v", got, want)
	}
	if v := maxVersion(t, raw); v != 21 {
		t.Fatalf("version %d", v)
	}
}

func TestMigrateDown22_RefusesUnless22IsNewest(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	raw := rawOpen(t, path)
	mustExec(t, raw, `INSERT INTO schema_version (version, name) VALUES (99, 'later')`)
	raw.Close()
	_, err := down22(t, path)
	if !errors.Is(err, db.ErrDownRefused) || !strings.Contains(err.Error(), "22 must be the newest") {
		t.Fatalf("down above 22: %v", err)
	}
	raw = rawOpen(t, path)
	mustExec(t, raw, `DELETE FROM schema_version WHERE version IN (22, 23)`)
	raw.Close()
	_, err = down22(t, path)
	if !errors.Is(err, db.ErrDownRefused) {
		t.Fatalf("down with a v21 version row and a leftover table: %v", err)
	}
	raw = rawOpen(t, path)
	if n := opCount(t, raw); n != 5 {
		t.Fatalf("a refused down touched the table: %d", n)
	}
	if matches, _ := filepath.Glob(path + ".pre-down-22-*"); len(matches) != 0 {
		t.Fatalf("a refused down wrote a backup: %v", matches)
	}
}

func TestMigrateDown22_AlreadyAt21IsNoop(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	if _, err := down22(t, path); err != nil {
		t.Fatal(err)
	}
	before := masterOf(t, path)
	out, err := down22(t, path)
	if err != nil || !strings.Contains(out, "already at schema 21") || !strings.Contains(out, "incidents=4") {
		t.Fatalf("second down: %q %v", out, err)
	}
	if matches, _ := filepath.Glob(path + ".pre-down-22-*.db"); len(matches) != 1 {
		t.Fatalf("backups after a no-op: %v", matches)
	}
	if after := masterOf(t, path); !slices.Equal(before, after) {
		t.Fatal("a no-op changed the schema")
	}
}

func TestMigrateDown22_RefusesWhileServiceHoldsTheDB(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	idle := openDB(t, path)
	_, err := down22(t, path)
	if !errors.Is(err, db.ErrDownRefused) || !strings.Contains(err.Error(), "stop media-fixer first") {
		t.Fatalf("down with an idle Open: %v", err)
	}
	idle.Close()
	if matches, _ := filepath.Glob(path + ".pre-down-22-*"); len(matches) != 0 {
		t.Fatalf("a refused down wrote a backup: %v", matches)
	}
	if v := maxVersion(t, rawOpen(t, path)); v != 27 {
		t.Fatalf("version %d", v)
	}
}

func TestMigrateDown22_WritesItsOwnBackup(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	out, err := down22(t, path)
	if err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(path + ".pre-down-22-*.db")
	if len(matches) != 1 || !strings.Contains(out, "backup: "+matches[0]) {
		t.Fatalf("backups %v, output %q", matches, out)
	}
	if ok, _ := filepath.Match("*.pre-down-22-[0-9]*T[0-9]*.db", filepath.Base(matches[0])); !ok {
		t.Fatalf("backup name %s", matches[0])
	}
	bak := rawOpen(t, matches[0])
	if n := opCount(t, bak); n != 5 {
		t.Fatalf("backup operations %d", n)
	}
	if v := maxVersion(t, bak); v != 22 {
		t.Fatalf("backup version %d", v)
	}
}

func TestMigrateDown22_ReUpgradeIgnoresOldOpIDs(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	raw := rawOpen(t, path)
	mustExec(t, raw, `INSERT INTO incident_events (incident_id, at, kind, payload) VALUES
		('inc-fail', ?, 'status_changed', '{"to":"manual_test_needed","op_id":4}')`, t20())
	raw.Close()
	if _, err := down22(t, path); err != nil {
		t.Fatal(err)
	}
	d := openDB(t, path)
	first := newOp(t, d, replaceSpecFor("inc-fail"))
	if first.ID != 1 {
		t.Fatalf("first op id after re-upgrade is %d", first.ID)
	}
	raw = rawOpen(t, path)
	var payload string
	err := raw.QueryRowContext(context.Background(),
		`SELECT payload FROM incident_events WHERE kind = 'status_changed'`).Scan(&payload)
	if err != nil || !strings.Contains(payload, `"op_id":4`) {
		t.Fatalf("old status_changed payload %q %v", payload, err)
	}
}

func TestMigrateDown22_JournalsUnfinishedIncidentOps(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	if _, err := down22(t, path); err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, path)
	got := strs(t, raw, `SELECT incident_id || '|' || json_extract(payload, '$.migration') || '|' ||
		json_extract(payload, '$.status') || '|' || json_array_length(payload, '$.steps') || '|' ||
		json_array_length(payload, '$.results') || '|' || source
		FROM incident_events WHERE kind = 'migration_cleared' ORDER BY incident_id`)
	want := []string{
		"inc-fail|-22|failed_at_step|2|2|migration",
		"inc-nores|-22|failed_at_step|2|2|migration",
		"inc-run|-22|running|2|2|migration",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("journal rows %v, want %v", got, want)
	}
}

func TestMigrateDown22_FailedDropLeavesNoEvent(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	raw := rawOpen(t, path)
	mustExec(t, raw, `CREATE TRIGGER block_version_delete BEFORE DELETE ON schema_version
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	raw.Close()
	if _, err := down22(t, path); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("down with a failing statement: %v", err)
	}
	raw = rawOpen(t, path)
	if n := clearedCount(t, raw); n != 0 {
		t.Fatalf("%d migration_cleared events survive a failed down", n)
	}
	if n := opCount(t, raw); n != 5 {
		t.Fatalf("operations after a failed down: %d", n)
	}
	if v := maxVersion(t, raw); v != 22 {
		t.Fatalf("version %d", v)
	}
}

func TestMigrateDown22_PrintsResumableCounts(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	out, err := down22(t, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"operations dropped: done=1 failed_at_step=3 running=1",
		"migration_cleared events: 3",
		"resumable dropped: incident=1 download=1",
		"schema_version max: 21",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	for _, leak := range []string{"Show S01", "/tv/e1.mkv", "dl-fail", "inc-fail"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q: %q", leak, out)
		}
	}
}

func TestMigrateDown21_RefusesWhile22Applied(t *testing.T) {
	t.Parallel()
	path := seededOps(t)
	var out bytes.Buffer
	err := db.MigrateDown(context.Background(), path, 21, &out)
	if !errors.Is(err, db.ErrDownRefused) || !strings.Contains(err.Error(), "21 must be the newest") {
		t.Fatalf("down 21 with 22 applied: %v", err)
	}
	if v := maxVersion(t, rawOpen(t, path)); v != 22 {
		t.Fatalf("version %d", v)
	}
}
