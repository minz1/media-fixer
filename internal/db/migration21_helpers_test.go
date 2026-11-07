package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

type row20 struct {
	id, status, what, item string
	finding                string
	locked, count          int
	po                     *string
	poNext                 *time.Time
	plan                   *string
	escalatedAfter         time.Duration
	bare                   bool
}

func t20() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

func oldStatuses() []string {
	return []string{
		"open", "investigating", "verifying", "reopened", "blocked", "agent_fixed", "manual_test_needed", "resolved",
	}
}

func queueStatuses() []string {
	return []string{
		"", "searching", "queued", "downloading", "completed", "importPending", "importBlocked", "paused", "delay",
		"failed", "warning",
	}
}

func poJSON(stage string, grabbed, keep bool) string {
	m := map[string]any{
		"media_type": "series", "title": "Show", "season": 1, "episode": 2,
		"started_at": t20(), "last_progress_at": t20(), "last_progress_pct": 0,
		"grab_notified": grabbed, "keep_searching": keep, "keep_searching_until": t20().Add(24 * time.Hour),
	}
	if stage != "" {
		m["last_stage"] = stage
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func schema20(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v20.db")
	if err := db.MigrateUpToForTest(path, 20); err != nil {
		t.Fatal(err)
	}
	return path, rawOpen(t, path)
}

func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open(
		"sqlite",
		path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_time_format=sqlite&_timezone=UTC",
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return raw
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func mustExec(t *testing.T, raw execer, q string, args ...any) {
	t.Helper()
	if _, err := raw.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func insertAll(t *testing.T, raw *sql.DB, rows []row20) {
	t.Helper()
	tx, err := raw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		insert20(t, tx, r)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func insert20(t *testing.T, raw execer, r row20) {
	t.Helper()
	var planAt any
	if r.plan != nil {
		planAt = t20()
	}
	var poNext any
	if r.poNext != nil {
		poNext = *r.poNext
	}
	mustExec(t, raw, `INSERT INTO incidents (id, created_at, updated_at, status, source, reported_by, what, title,
		jellyfin_item_id, finding, recommended_actions, action_count, autonomous_locked, pending_outcome,
		pending_outcome_next_check, escalation_plan, escalation_plan_at)
		VALUES (?, ?, ?, ?, 'discord', 'someone', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.id, t20(), t20(), r.status, r.what, "Title "+r.id, r.item, r.finding, r.finding, r.count, r.locked,
		r.po, poNext, r.plan, planAt)
	if r.bare {
		return
	}
	mustExec(t, raw, `INSERT INTO incident_reporters (incident_id, reporter, source, reported_at)
		VALUES (?, 'someone', 'discord', ?)`, r.id, t20())
	mustExec(t, raw, `INSERT INTO actions_log (id, incident_id, action, triggered_by, status, applied_at)
		VALUES (?, ?, 'jellyfin_library_scan', 'agent', 'applied', ?)`, "a-"+r.id, r.id, t20())
	mustExec(
		t,
		raw,
		`INSERT INTO incident_events (incident_id, at, kind, payload) VALUES (?, ?, 'incident_created', '{}')`,
		r.id,
		t20(),
	)
	mustExec(t, raw, `INSERT INTO incident_seerr_issues (incident_id, issue_id, added_at) VALUES (?, ?, ?)`,
		r.id, "s-"+r.id, t20())
	if r.escalatedAfter > 0 {
		mustExec(t, raw, `INSERT INTO incident_events (incident_id, at, kind, payload)
			VALUES (?, ?, 'status_changed', '{"status":"manual_test_needed"}')`, r.id, t20().Add(r.escalatedAfter))
	}
}

func everyPO() []string {
	var out []string
	for _, qs := range queueStatuses() {
		for _, grabbed := range []bool{false, true} {
			for _, keep := range []bool{false, true} {
				out = append(out, poJSON(qs, grabbed, keep))
			}
		}
	}
	return out
}

func syntheticRows() []row20 {
	var rows []row20
	n := 0
	add := func(r row20) {
		n++
		r.id = fmt.Sprintf("inc-%04d", n)
		if r.what == "" {
			r.what = "cant_play"
		}
		rows = append(rows, r)
	}
	plan := `{"files":[]}`
	for _, st := range oldStatuses() {
		for _, locked := range []int{0, 1} {
			for _, p := range []*string{nil, &plan} {
				add(row20{status: st, locked: locked, plan: p, count: 2 * locked})
				add(row20{status: st, locked: locked, plan: p, what: "stuck_read", item: "item-1"})
				for _, po := range everyPO() {
					next := t20().Add(time.Hour)
					add(row20{status: st, locked: locked, plan: p, po: &po, poNext: &next})
				}
			}
		}
	}
	for _, after := range []time.Duration{time.Hour, 13 * time.Hour} {
		po := poJSON("searching", false, false)
		add(row20{status: "manual_test_needed", po: &po, escalatedAfter: after})
	}
	bad := `{not json`
	add(row20{status: "manual_test_needed", po: &bad})
	add(row20{status: "verifying", po: &bad})
	return rows
}

type want21 struct {
	status, reason string
	poKept         bool
	planKept       bool
	locked         bool
	stage, queue   string
}

func stageOf(po string) string {
	var m map[string]any
	if json.Unmarshal([]byte(po), &m) != nil {
		return ""
	}
	s, _ := m["last_stage"].(string)
	return s
}

func grabbedOf(po string) bool {
	var m map[string]any
	_ = json.Unmarshal([]byte(po), &m)
	g, _ := m["grab_notified"].(bool)
	return g
}

func validJSON(s string) bool { return json.Valid([]byte(s)) }

func expect21(r row20) want21 {
	w := want21{status: r.status, locked: r.locked == 1 || r.status == "blocked"}
	w.planKept = r.plan != nil && r.status == "manual_test_needed"
	hasPO := r.po != nil && validJSON(*r.po)
	if hasPO {
		raw := stageOf(*r.po)
		w.stage, w.queue = "downloading", raw
		if raw == "" || raw == "searching" {
			w.stage, w.queue = "searching", ""
		}
	}
	switch {
	case r.status == "verifying" && hasPO:
		w.status, w.poKept = "awaiting_download", true
	case r.status == "manual_test_needed" && r.po == nil:
		w.reason = "needs_owner"
	case r.status == "manual_test_needed" && hasPO && !grabbedOf(*r.po) && w.stage == "searching" &&
		r.escalatedAfter <= 12*time.Hour:
		w.reason, w.poKept = "no_release", true
	case r.status == "manual_test_needed":
		w.reason = "download_problem"
	}
	return w
}

func childCounts(t *testing.T, raw *sql.DB) [5]int {
	t.Helper()
	var out [5]int
	for i, table := range []string{"incident_events", "actions_log", "incident_reporters", "incident_seerr_issues",
		"discord_cards"} {
		if err := raw.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func master(t *testing.T, raw *sql.DB) []string {
	t.Helper()
	return strs(t, raw, `SELECT type || '|' || name || '|' || tbl_name || '|' || COALESCE(sql, '') FROM sqlite_master
		 WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
}

func strs(t *testing.T, raw *sql.DB, q string) []string {
	t.Helper()
	var out []string
	eachRow(t, raw, func(rows *sql.Rows) error {
		var s string
		err := rows.Scan(&s)
		out = append(out, s)
		return err
	}, q)
	return out
}

func openDB(t *testing.T, path string) *db.DB {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func eachRow(t *testing.T, raw *sql.DB, scan func(rows *sql.Rows) error, q string, args ...any) {
	t.Helper()
	rows, err := raw.QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err = scan(rows); err != nil {
			t.Fatal(err)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
