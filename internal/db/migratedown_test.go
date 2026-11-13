package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
)

const (
	v20GetIncident = `SELECT id, created_at, updated_at, status, source, reported_by, what, title,
		COALESCE(jellyfin_item_id,''), COALESCE(details,''), COALESCE(finding,''), COALESCE(recommended_actions,''),
		action_count, autonomous_locked FROM incidents WHERE id = ?`
	v20ListIncidents = `SELECT id, created_at, updated_at, status, source, reported_by, what, title,
		COALESCE(jellyfin_item_id,''), COALESCE(details,''), COALESCE(finding,''), COALESCE(recommended_actions,''),
		action_count, autonomous_locked FROM incidents ORDER BY created_at DESC LIMIT ? OFFSET ?`
	v20DuePendingOutcomes = `SELECT id, created_at, updated_at, status, source, reported_by, what, title,
		COALESCE(jellyfin_item_id,''), COALESCE(details,''), COALESCE(finding,''), COALESCE(recommended_actions,''),
		action_count, autonomous_locked FROM incidents
		WHERE status = ? AND pending_outcome IS NOT NULL AND pending_outcome_next_check <= ?`
	v20GetPendingOutcome = `SELECT pending_outcome FROM incidents WHERE id = ?`
	v20GetEscalationPlan = `SELECT escalation_plan, escalation_plan_at FROM incidents WHERE id = ?`
	snapshotIncidents    = 78
	snapshotLocked       = 5
	snapshotOverBudget   = 8
	snapshotEvents       = 767
	snapshotActions      = 86
	snapshotReporters    = 82
	snapshotSeerr        = 5
	snapshotCards        = 16
)

func edgeRows() []row20 {
	str := func(s string) *string { return &s }
	next := t20()
	rows := []row20{
		{status: "resolved", po: str(poJSON("searching", false, false))},
		{status: "verifying", po: str(poJSON("queued", true, false)), poNext: &next},
		{status: "verifying"},
		{status: "verifying", what: "stuck_read", item: "it"},
		{status: "blocked"},
		{status: "manual_test_needed", plan: str(`{"files":[]}`),
			finding: `{"escalate_action":"remove_and_search","root_cause":"x"}`},
		{status: "manual_test_needed", po: str(poJSON("searching", false, true))},
		{status: "manual_test_needed", po: str(poJSON("searching", false, false)), escalatedAfter: 13 * 60 * 60e9},
	}
	for _, qs := range queueStatuses() {
		rows = append(
			rows,
			row20{status: "manual_test_needed", po: str(poJSON(qs, qs != "" && qs != "searching", false))},
		)
	}
	return rows
}

func roundTripDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, raw := schema20(t)
	rows := edgeRows()
	for len(rows) < snapshotIncidents {
		rows = append(rows, row20{status: "resolved"})
	}
	for i := range rows {
		rows[i].id = fmt.Sprintf("rt-%04d", i)
		rows[i].bare = true
		if rows[i].what == "" {
			rows[i].what = "cant_play"
		}
		if i < snapshotLocked {
			rows[i].locked = 1
		}
		if i < snapshotOverBudget {
			rows[i].count = 4
		}
	}
	insertAll(t, raw, rows)
	ctx := context.Background()
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := func(i int) string { return rows[i%len(rows)].id }
	for i := range snapshotEvents {
		mustExec(t, tx, `INSERT INTO incident_events (incident_id, at, kind, payload) VALUES (?, ?, 'llm_round', '{}')`,
			id(i), t20())
	}
	for i := range snapshotActions {
		mustExec(t, tx, `INSERT INTO actions_log (id, incident_id, action, triggered_by, status)
			VALUES (?, ?, 'scan', 'agent', 'applied')`, fmt.Sprintf("act-%d", i), id(i))
	}
	for i := range snapshotReporters {
		mustExec(t, tx, `INSERT INTO incident_reporters (incident_id, reporter, source) VALUES (?, ?, 'discord')`,
			id(i), fmt.Sprintf("user-%d", i))
	}
	for i := range snapshotSeerr {
		mustExec(t, tx, `INSERT INTO incident_seerr_issues (incident_id, issue_id) VALUES (?, ?)`, id(i), "s")
	}
	for i := range snapshotCards {
		mustExec(t, tx, `INSERT INTO discord_cards (subject, channel_id, message_id, card, sent_at)
			VALUES (?, 'c', ?, '{}', ?)`, "inc:"+id(i), fmt.Sprintf("m%d", i), t20())
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func seedHalfRun(t *testing.T, raw *sql.DB) []string {
	t.Helper()
	esc := `{"escalate_action":"remove_and_search"}`
	mustExec(t, raw, `UPDATE incidents SET status = 'open', parked = 1 WHERE id = 'rt-0070'`)
	mustExec(t, raw, `UPDATE incidents SET status = 'executing_plan', finding = ?, recommended_actions = ?,
		escalation_plan = '{"files":[]}', escalation_plan_at = ? WHERE id = 'rt-0071'`, esc, esc, t20())
	mustExec(t, raw, `UPDATE incidents SET status = 'manual_test_needed', reason = 'plan_interrupted', finding = ?,
		recommended_actions = ? WHERE id = 'rt-0072'`, esc, esc)
	mustExec(t, raw, `UPDATE incidents SET status = 'manual_test_needed', reason = 'plan_partial', finding = ?,
		recommended_actions = 'not json' WHERE id = 'rt-0073'`, esc)
	return []string{"rt-0071", "rt-0072", "rt-0073"}
}

func migrateDown(t *testing.T, path string) string {
	t.Helper()
	strip22(t, path)
	var out bytes.Buffer
	if err := db.MigrateDown(context.Background(), path, 21, &out); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	return out.String()
}

func scanV20(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var ids []string
	eachRow(t, raw, func(rows *sql.Rows) error {
		var (
			id, s                                     string
			c, u                                      sql.NullTime
			n, l                                      int
			src, rep, what, title, item, det, f, acts string
		)
		err := rows.Scan(&id, &c, &u, &s, &src, &rep, &what, &title, &item, &det, &f, &acts, &n, &l)
		ids = append(ids, id)
		return err
	}, v20ListIncidents, 1000, 0)
	for _, id := range ids {
		var po, plan sql.NullString
		var at sql.NullTime
		if err := raw.QueryRowContext(ctx, v20GetPendingOutcome, id).Scan(&po); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRowContext(ctx, v20GetEscalationPlan, id).Scan(&plan, &at); err != nil {
			t.Fatal(err)
		}
		cols := make([]any, 14)
		for i := range cols {
			cols[i] = new(any)
		}
		if err := raw.QueryRowContext(ctx, v20GetIncident, id).Scan(cols...); err != nil {
			t.Fatal(err)
		}
	}
	eachRow(t, raw, func(*sql.Rows) error { return nil }, v20DuePendingOutcomes, "verifying", t20().AddDate(1, 0, 0))
}

func escalateAction(t *testing.T, raw *sql.DB, id string) (string, bool) {
	t.Helper()
	var action sql.NullString
	var plan sql.NullString
	if err := raw.QueryRowContext(context.Background(), `SELECT json_extract(finding, '$.escalate_action'),
		escalation_plan FROM incidents WHERE id = ?`, id).Scan(&action, &plan); err != nil {
		t.Fatal(err)
	}
	return action.String, plan.Valid
}

func TestMigration21_RoundTrip(t *testing.T) {
	t.Parallel()
	path, raw := roundTripDB(t)
	before, children := master(t, raw), childCounts(t, raw)
	openDB(t, path).Close()
	halfRun := seedHalfRun(t, raw)
	raw.Close()
	out := migrateDown(t, path)
	raw = rawOpen(t, path)
	if after := master(t, raw); !slices.Equal(before, after) {
		t.Fatalf("sqlite_master after down differs:\n%v\n%v", before, after)
	}
	got := childCounts(t, raw)
	if !slices.Equal(got[1:], children[1:]) || got[0] <= children[0] {
		t.Fatalf("children %v -> %v", children, got)
	}
	scanV20(t, raw)
	for _, id := range halfRun {
		action, hasPlan := escalateAction(t, raw, id)
		if action != "manual_investigation" || hasPlan {
			t.Errorf("%s: escalate_action %q plan %v", id, action, hasPlan)
		}
	}
	if len(clearedValues(t, raw, "finding")) != len(halfRun) {
		t.Errorf("findings journalled: %v", clearedValues(t, raw, "finding"))
	}
	var st string
	if err := raw.QueryRowContext(context.Background(), `SELECT status FROM incidents WHERE id = 'rt-0070'`).
		Scan(&st); err != nil || st != "reopened" {
		t.Errorf("parked open came back as %q %v", st, err)
	}
	if !strings.Contains(out, "changed rows: 6") {
		t.Errorf("down output %q", out)
	}
	raw.Close()
	if again := migrateDown(t, path); !strings.Contains(again, "already at schema 20") {
		t.Fatalf("second down: %q", again)
	}
	openDB(t, path).Close()
	raw = rawOpen(t, path)
	if n := childCounts(t, raw); !slices.Equal(n[1:], children[1:]) {
		t.Fatalf("second up children %v", n)
	}
}

func rewriteIncidentsDDL(t *testing.T, raw *sql.DB) {
	t.Helper()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	for _, q := range []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE incidents_new (id TEXT PRIMARY KEY, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, status TEXT NOT NULL DEFAULT 'open', source TEXT NOT NULL,
reported_by TEXT NOT NULL, what TEXT NOT NULL, title TEXT NOT NULL, jellyfin_item_id TEXT, details TEXT, finding TEXT,
recommended_actions TEXT, action_count INTEGER NOT NULL DEFAULT 0, autonomous_locked INTEGER NOT NULL DEFAULT 0,
pending_outcome TEXT, pending_outcome_next_check DATETIME, escalation_plan TEXT, escalation_plan_at DATETIME,
last_reporter_note_at DATETIME)`,
		`INSERT INTO incidents_new SELECT * FROM incidents`,
		`DROP TABLE incidents`,
		`ALTER TABLE incidents_new RENAME TO incidents`,
		`CREATE INDEX idx_incidents_status ON incidents (status)`,
		`CREATE INDEX idx_incidents_title ON incidents (title)`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err = conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestMigration21_DownRestoresSavedMaster(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	insertAll(t, raw, syntheticRows()[:10])
	rewriteIncidentsDDL(t, raw)
	before := master(t, raw)
	openDB(t, path).Close()
	raw.Close()
	migrateDown(t, path)
	raw = rawOpen(t, path)
	if after := master(t, raw); !slices.Equal(before, after) {
		t.Fatalf("saved DDL not restored:\n%v\n%v", before, after)
	}
}

func TestMigration21_DownThenUp(t *testing.T) {
	t.Parallel()
	path, raw := roundTripDB(t)
	raw.Close()
	single := filepath.Join(t.TempDir(), "single.db")
	copyFile(t, path, single)
	openDB(t, single).Close()
	openDB(t, path).Close()
	migrateDown(t, path)
	openDB(t, path).Close()
	a, b := rawOpen(t, single), rawOpen(t, path)
	if ma, mb := master(t, a), master(t, b); !slices.Equal(ma, mb) {
		t.Fatalf("down then up differs from a single up:\n%v\n%v", ma, mb)
	}
	ca, cb := childCounts(t, a), childCounts(t, b)
	if !slices.Equal(ca[1:], cb[1:]) {
		t.Fatalf("children %v vs %v", ca, cb)
	}
	var saved int
	if err := b.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_saved_ddl`).
		Scan(&saved); err != nil ||
		saved != 3 {
		t.Fatalf("saved ddl rows %d %v", saved, err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func upgraded(t *testing.T) string {
	t.Helper()
	path, raw := schema20(t)
	insertAll(t, raw, syntheticRows()[:30])
	raw.Close()
	openDB(t, path).Close()
	return path
}

func TestMigrateDown_RefusesWhileServiceHoldsTheDB(t *testing.T) {
	t.Parallel()
	path := upgraded(t)
	idle := openDB(t, path)
	var out bytes.Buffer
	err := db.MigrateDown(context.Background(), path, 21, &out)
	if !errors.Is(err, db.ErrDownRefused) || !strings.Contains(err.Error(), "stop media-fixer first") {
		t.Fatalf("down with an idle Open: %v", err)
	}
	idle.Close()
	if matches, _ := filepath.Glob(path + ".pre-down-21-*"); len(matches) != 0 {
		t.Fatalf("a refused down wrote a backup: %v", matches)
	}
}

func TestMigrateDown_WritesBackupFirst(t *testing.T) {
	t.Parallel()
	path := upgraded(t)
	out := migrateDown(t, path)
	matches, _ := filepath.Glob(path + ".pre-down-21-*.db")
	if len(matches) != 1 || !strings.Contains(out, "backup: "+matches[0]) {
		t.Fatalf("backups %v, output %q", matches, out)
	}
	bak := rawOpen(t, matches[0])
	var v, n int
	if err := bak.QueryRowContext(context.Background(), `SELECT MAX(version) FROM schema_version`).
		Scan(&v); err != nil ||
		v != 21 {
		t.Fatalf("backup version %d %v", v, err)
	}
	if err := bak.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM incidents`).
		Scan(&n); err != nil ||
		n != 30 {
		t.Fatalf("backup rows %d %v", n, err)
	}
}

func TestMigrateDown_Idempotent(t *testing.T) {
	t.Parallel()
	path := upgraded(t)
	migrateDown(t, path)
	out := migrateDown(t, path)
	if !strings.Contains(out, "already at schema 20") || !strings.Contains(out, "incidents=30") {
		t.Fatalf("second down: %q", out)
	}
}

func TestMigrateDown_RefusesAbove21(t *testing.T) {
	t.Parallel()
	path := upgraded(t)
	raw := rawOpen(t, path)
	mustExec(t, raw, `INSERT INTO schema_version (version, name) VALUES (99, 'later')`)
	raw.Close()
	var out bytes.Buffer
	err := db.MigrateDown(context.Background(), path, 21, &out)
	if !errors.Is(err, db.ErrDownRefused) || !strings.Contains(err.Error(), "21 must be the newest") {
		t.Fatalf("down above 21: %v", err)
	}
}

func TestMigrateDown_PrintsCountsChangedRowsAndIdsOnly(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	po := poJSON("queued", true, false)
	insertAll(t, raw, []row20{
		{id: "keep-me", status: "resolved", what: "cant_play"},
		{id: "moved-1", status: "verifying", what: "cant_play", po: &po},
		{id: "moved-2", status: "verifying", what: "cant_play"},
	})
	raw.Close()
	openDB(t, path).Close()
	out := migrateDown(t, path)
	for _, want := range []string{"before: incidents=3", "after: incidents=3", "changed rows: 2", "moved-1", "moved-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	for _, leak := range []string{"Title", "someone", "keep-me", "Show"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q: %q", leak, out)
		}
	}
}

func strip26(t *testing.T, path string) {
	t.Helper()
	raw := rawOpen(t, path)
	defer raw.Close()
	if maxVersion(t, raw) != 26 {
		return
	}
	mustExec(t, raw, `DROP TABLE notify_outbox`)
	mustExec(t, raw, `ALTER TABLE incidents DROP COLUMN last_seerr_note_at`)
	mustExec(t, raw, `DELETE FROM schema_version WHERE version = 26`)
}

func strip25(t *testing.T, path string) {
	t.Helper()
	strip26(t, path)
	raw := rawOpen(t, path)
	defer raw.Close()
	if maxVersion(t, raw) != 25 {
		return
	}
	mustExec(t, raw, `ALTER TABLE sweep_refused DROP COLUMN code`)
	mustExec(t, raw, `DELETE FROM schema_version WHERE version = 25`)
}

func strip24(t *testing.T, path string) {
	t.Helper()
	strip25(t, path)
	raw := rawOpen(t, path)
	defer raw.Close()
	if maxVersion(t, raw) != 24 {
		return
	}
	mustExec(t, raw, `DROP TABLE stuck_downloads`)
	mustExec(t, raw, `ALTER TABLE incidents DROP COLUMN stuck_read_reran`)
	mustExec(t, raw, `DELETE FROM schema_version WHERE version = 24`)
}

func strip23(t *testing.T, path string) {
	t.Helper()
	strip24(t, path)
	raw := rawOpen(t, path)
	defer raw.Close()
	if maxVersion(t, raw) != 23 {
		return
	}
	mustExec(t, raw, `ALTER TABLE discord_cards DROP COLUMN last_update`)
	mustExec(t, raw, `ALTER TABLE discord_cards DROP COLUMN state`)
	mustExec(t, raw, `DELETE FROM schema_version WHERE version = 23`)
}

func strip22(t *testing.T, path string) {
	t.Helper()
	strip23(t, path)
	raw := rawOpen(t, path)
	v := maxVersion(t, raw)
	raw.Close()
	if v != 22 {
		return
	}
	var out bytes.Buffer
	if err := db.MigrateDown(context.Background(), path, 22, &out); err != nil {
		t.Fatalf("migrate down 22: %v", err)
	}
}
