package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
)

func swapSpecFor(id string) operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: id}, Kind: operation.KindSwapRelease,
		Title: "Show S01E06",
		Steps: []operation.Step{
			{
				Kind: operation.StepImport, App: "sonarr", Path: "/data/downloads/sonarr/r/e6.mkv", Size: 10,
				FileID: 4, MediaID: 7, EpisodeIDs: []int{66}, Season: 1,
			},
			{
				Kind: operation.StepJellyfinNotify, App: "jellyfin", Path: "/media/tv/Show/e6.mkv", Name: "jf-1",
				Season: 1, Episode: 6,
			},
		},
	}
}

func TestD2_SwapKindStored(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	addIncident(t, d, "inc-swap")
	op := newOp(t, d, swapSpecFor("inc-swap"))
	got, err := d.Operation(context.Background(), op.ID)
	if err != nil || got.Kind != operation.KindSwapRelease {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestD2_Migration27DDL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ddl.db")
	if err := db.MigrateUpToForTest(path, 26); err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, path)
	want := strs(t, raw, `SELECT sql FROM sqlite_master WHERE name = 'operations'`)[0]
	mustExec(t, raw, `INSERT INTO incidents (id, source, reported_by, what, title) VALUES ('a', 't', 't', 'x', 'T')`)
	for i := range 3 {
		mustExec(t, raw, `INSERT INTO operations (subject_kind, subject_id, kind, source, steps, results, step_index,
			status, created_at, updated_at) VALUES ('incident', ?, 'replace', '{}', '[{},{}]', '[{},{}]', 0, 'running',
			?, ?)`, "i"+string(rune('a'+i)), opAt(0), opAt(0))
	}
	mustExec(t, raw, `DELETE FROM operations WHERE id = 3`)
	seq := strs(t, raw, `SELECT seq FROM sqlite_sequence WHERE name = 'operations'`)
	before := master(t, raw)
	raw.Close()
	openDB(t, path).Close()
	raw = rawOpen(t, path)
	got := strs(t, raw, `SELECT sql FROM sqlite_master WHERE name = 'operations'`)[0]
	if got != strings.Replace(want, `'regrab')),`, `'regrab', 'swap_release')),`, 1) || got == want {
		t.Fatalf("ddl:\n%s", got)
	}
	if n := strs(
		t,
		raw,
		`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('operations_subject', 'operations_one_running')`,
	); n[0] != "2" {
		t.Fatalf("indexes %v", n)
	}
	if after := strs(
		t,
		raw,
		`SELECT seq FROM sqlite_sequence WHERE name = 'operations'`,
	); strings.Join(
		after,
		"",
	) != strings.Join(
		seq,
		"",
	) {
		t.Fatalf("sequence %v -> %v", seq, after)
	}
	if after := master(t, raw); len(after) != len(before) {
		t.Fatalf("objects %d -> %d", len(before), len(after))
	}
	if n := strs(t, raw, `SELECT COUNT(*) FROM operations`); n[0] != "2" {
		t.Fatalf("rows %v", n)
	}
}

func seededSwaps(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "swap.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"inc-done", "inc-run", "inc-keep", "inc-ver", "inc-find"} {
		addIncident(t, d, id)
	}
	done := newOp(t, d, swapSpecFor("inc-done"))
	step(t, d, done.ID, startEv(0), finishEv(0, operation.ResultDone), startEv(1), finishEv(1, operation.ResultDone))
	newOp(t, d, swapSpecFor("inc-run"))
	newOp(t, d, replaceSpecFor("inc-keep"))
	d.Close()
	raw := rawOpen(t, path)
	defer raw.Close()
	mustExec(t, raw, `UPDATE incidents SET status = 'verifying', verify_next_check = ?,
		verify_spec = '{"kind":"subtitles","item_id":"x","swap":{"season":1}}' WHERE id = 'inc-ver'`, opAt(0))
	mustExec(t, raw, `UPDATE incidents SET finding = '{"escalate_action":"swap_release"}' WHERE id = 'inc-find'`)
	return path
}

func opRows(t *testing.T, raw *sql.DB) []string {
	t.Helper()
	return strs(
		t,
		raw,
		`SELECT id || '|' || kind || '|' || subject_id || '|' || status || '|' || steps || '|' || results
		|| '|' || version || '|' || created_at || '|' || updated_at FROM operations WHERE kind <> 'swap_release'
		ORDER BY id`,
	)
}

func TestD2_MigrateDown27(t *testing.T) {
	t.Parallel()
	path := seededSwaps(t)
	raw := rawOpen(t, path)
	keep := opRows(t, raw)
	seq := strs(t, raw, `SELECT seq FROM sqlite_sequence WHERE name = 'operations'`)
	incidents := strs(t, raw, `SELECT id || status || COALESCE(finding, '') FROM incidents ORDER BY id`)
	raw.Close()
	var out bytes.Buffer
	if err := db.MigrateDown(context.Background(), path, 27, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"running swap dropped: Show S01E06 series 7 S01E06", "swap operations deleted: 2",
		"migration_cleared events: 1", "incidents with a swap verify or finding: 2 inc-find inc-ver",
		"dismiss or re-triage these incidents", "schema_version max: 26", "backup: ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	raw = rawOpen(t, path)
	if v := maxVersion(t, raw); v != 26 {
		t.Fatalf("version %d", v)
	}
	if got := opRows(t, raw); strings.Join(got, "\n") != strings.Join(keep, "\n") || len(got) != 1 {
		t.Fatalf("non-swap rows changed:\n%v\n%v", keep, got)
	}
	if got := strs(t, raw, `SELECT seq FROM sqlite_sequence WHERE name = 'operations'`); got[0] != seq[0] {
		t.Fatalf("sequence %v -> %v", seq, got)
	}
	if got := strs(
		t,
		raw,
		`SELECT id || status || COALESCE(finding, '') FROM incidents ORDER BY id`,
	); strings.Join(
		got,
		"|",
	) != strings.Join(
		incidents,
		"|",
	) {
		t.Fatalf("incidents changed: %v", got)
	}
	if n := strs(t, raw, `SELECT COUNT(*) FROM incident_events WHERE kind = 'migration_cleared'
		AND incident_id = 'inc-run' AND json_extract(payload, '$.status') = 'running'`); n[0] != "1" {
		t.Fatalf("journal %v", n)
	}
	if n := strs(t, raw, `SELECT COUNT(*) FROM incident_events WHERE kind = 'migration_cleared'`); n[0] != "1" {
		t.Fatalf("done op journaled: %v", n)
	}
	if strings.Contains(strs(t, raw, `SELECT sql FROM sqlite_master WHERE name = 'operations'`)[0], "swap_release") {
		t.Fatal("old check not restored")
	}
	mustExec(
		t,
		raw,
		`INSERT INTO operations (subject_kind, subject_id, kind, source, steps, results, step_index, status,
		created_at, updated_at) VALUES ('incident', 'inc-done', 'replace', '{}', '[{},{}]', '[{},{}]', 0, 'running', ?, ?)`,
		opAt(0),
		opAt(0),
	)
	if got := strs(t, raw, `SELECT MAX(id) FROM operations`); got[0] <= seq[0] {
		t.Fatalf("id reused: %v vs %v", got, seq)
	}
}

func TestD2_MigrateDown27NoSwapOpsAndRefusal(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "none.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw := rawOpen(t, path)
	mustExec(t, raw, `INSERT INTO schema_version (version, name) VALUES (28, 'later')`)
	raw.Close()
	var out bytes.Buffer
	if err = db.MigrateDown(
		context.Background(),
		path,
		27,
		&out,
	); err == nil ||
		!strings.Contains(err.Error(), "27 must be the newest") {
		t.Fatalf("later migration: %v", err)
	}
	raw = rawOpen(t, path)
	mustExec(t, raw, `DELETE FROM schema_version WHERE version = 28`)
	raw.Close()
	if err = db.MigrateDown(
		context.Background(),
		path,
		27,
		&out,
	); err != nil ||
		!strings.Contains(out.String(), "swap operations deleted: 0") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	out.Reset()
	if err = db.MigrateDown(
		context.Background(),
		path,
		27,
		&out,
	); err != nil ||
		!strings.Contains(out.String(), "already at schema 26") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}
