package db_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func t24() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

func v23DB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v23.db")
	if err := db.MigrateUpToForTest(path, 23); err != nil {
		t.Fatal(err)
	}
	return path, rawOpen(t, path)
}

func migrated24(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m24.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, rawOpen(t, path)
}

const bareStateInsert = `INSERT INTO stuck_downloads (app, download_id, state, first_seen)
	VALUES ('sonarr', 'x', ?, CURRENT_TIMESTAMP)`

const reranColumns = `SELECT COUNT(*) FROM pragma_table_info('incidents') WHERE name = 'stuck_read_reran'`

const oldInsert = `INSERT INTO stuck_queue_downloads (app, download_id, title, verdict, first_seen_at, notified_at)
	VALUES (?, ?, ?, ?, ?, ?)`

func TestMigration24_SeedsFromOldTable(t *testing.T) {
	t.Parallel()
	path, raw := v23DB(t)
	first := t24().Add(-48 * time.Hour)
	mustExec(t, raw, oldInsert, "sonarr", "N", "Show", "gap", first, t24().Add(-40*time.Hour))
	mustExec(t, raw, oldInsert, "radarr", "W", "Film", "clear", first, nil)
	mustExec(t, raw, oldInsert, "lidarr", "B", "Album", "gap", first, nil)
	raw.Close()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	n, ok, err := d.GetStuck(ctx, "sonarr", "N")
	if err != nil || !ok {
		t.Fatalf("notified row: %v %v", ok, err)
	}
	if n.State != "notified" || !n.FirstSeen.Equal(first) || !n.NotifiedAt.Equal(t24().Add(-40*time.Hour)) ||
		n.Offered != "" || n.TargetsFP != "" || n.Misses != 0 || n.ActedOpID != 0 {
		t.Errorf("notified row = %+v", n)
	}
	w, ok, err := d.GetStuck(ctx, "radarr", "W")
	if err != nil || !ok {
		t.Fatalf("waiting row: %v %v", ok, err)
	}
	if w.State != "waiting" || w.Offered != "remove" || !w.NotifiedAt.IsZero() || !w.FirstSeen.Equal(first) {
		t.Errorf("waiting row = %+v", w)
	}
	if _, ok, err = d.GetStuck(ctx, "lidarr", "B"); err != nil || ok {
		t.Errorf("bad-app row carried over: %v %v", ok, err)
	}
}

func TestMigration24_SeedFailureAbortsMigration(t *testing.T) {
	t.Parallel()
	path, raw := v23DB(t)
	mustExec(t, raw, `DROP TABLE stuck_queue_downloads`)
	mustExec(t, raw, `CREATE TABLE stuck_queue_downloads (app TEXT NOT NULL, download_id TEXT NOT NULL,
		title TEXT NOT NULL, verdict TEXT NOT NULL, first_seen_at DATETIME, notified_at DATETIME,
		PRIMARY KEY (app, download_id))`)
	mustExec(t, raw, oldInsert, "sonarr", "OK", "Show", "gap", t24(), nil)
	mustExec(t, raw, oldInsert, "sonarr", "BAD", "Show", "gap", nil, nil)
	raw.Close()
	if d, err := db.Open(path); err == nil {
		d.Close()
		t.Fatal("Open succeeded on a seed the CHECKs refuse")
	}
	raw = rawOpen(t, path)
	if v := countInt(t, raw, `SELECT MAX(version) FROM schema_version`); v != 23 {
		t.Errorf("schema version %d, want 23", v)
	}
	if n := countInt(t, raw, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'stuck_downloads'`); n != 0 {
		t.Errorf("stuck_downloads left behind")
	}
	if n := countInt(t, raw, reranColumns); n != 0 {
		t.Errorf("incidents column left behind")
	}
}

func oldDump(t *testing.T, raw *sql.DB) []string {
	t.Helper()
	rows, err := raw.QueryContext(context.Background(), `SELECT app || '|' || download_id || '|' || title || '|' ||
		verdict || '|' || first_seen_at || '|' || COALESCE(notified_at, '-') FROM stuck_queue_downloads ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigration24_OldTableUntouched(t *testing.T) {
	t.Parallel()
	path, raw := v23DB(t)
	mustExec(t, raw, oldInsert, "sonarr", "N", "Show", "gap", t24(), t24().Add(time.Hour))
	mustExec(t, raw, oldInsert, "radarr", "W", "Film", "clear", t24(), nil)
	mustExec(t, raw, oldInsert, "lidarr", "B", "Album", "gap", t24(), nil)
	before := oldDump(t, raw)
	raw.Close()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw = rawOpen(t, path)
	if after := oldDump(t, raw); len(before) != 3 || !slices.Equal(before, after) {
		t.Errorf("old table changed:\n%v\n%v", before, after)
	}
}

func stuckInsert(state, cols, vals string) string {
	return `INSERT INTO stuck_downloads (app, download_id, state, first_seen, ` + cols + `)
		VALUES ('sonarr', 'x', '` + state + `', CURRENT_TIMESTAMP, ` + vals + `)`
}

func TestMigration24_ChecksRejectIllegalRows(t *testing.T) {
	t.Parallel()
	_, raw := migrated24(t)
	cases := []struct{ name, constraint, query string }{
		{"app", "stuck_app", `INSERT INTO stuck_downloads (app, download_id, state, first_seen, offered)
			VALUES ('lidarr', 'x', 'waiting', CURRENT_TIMESTAMP, 'remove')`},
		{"state", "stuck_state", stuckInsert("bogus", "offered", "''")},
		{"misses", "stuck_misses", stuckInsert("waiting", "offered, misses", "'remove', 2")},
		{"offered", "stuck_offered", stuckInsert("held", "offered", "'bogus'")},
		{"waiting without remove", "stuck_offered", stuckInsert("waiting", "offered", "''")},
		{"notified without notified_at", "stuck_notified", stuckInsert("notified", "offered", "''")},
		{"import command outside import_inflight", "stuck_import", stuckInsert("waiting",
			"offered, import_command_id, import_started_at", "'remove', 7, CURRENT_TIMESTAMP")},
		{"import files over total", "stuck_import_files", stuckInsert("waiting",
			"offered, import_files, import_total", "'remove', 5, 3")},
		{"acted partly set", "stuck_acted_whole", stuckInsert("held", "offered, acted_action", "'remove', 'remove'")},
		{"acted values", "stuck_acted_values", stuckInsert("notified",
			"offered, notified_at, acted_action, acted_at, acted_result",
			"'', CURRENT_TIMESTAMP, 'bogus', CURRENT_TIMESTAMP, 'ok'")},
		{"acted with every acted_* NULL", "stuck_acted_owner", stuckInsert("acted", "offered", "''")},
		{"acted on a held row", "stuck_acted_owner", stuckInsert("held",
			"offered, acted_action, acted_at, acted_result", "'remove', 'remove', CURRENT_TIMESTAMP, 'ok'")},
		{"notified with a non-import action", "stuck_acted_owner", stuckInsert("notified",
			"offered, notified_at, acted_action, acted_at, acted_result",
			"'', CURRENT_TIMESTAMP, 'regrab', CURRENT_TIMESTAMP, 'ok'")},
		{"unfinished import", "stuck_unfinished_regrab", stuckInsert("acted",
			"offered, acted_action, acted_at, acted_result, import_files, import_total",
			"'', 'import', CURRENT_TIMESTAMP, 'unfinished', 1, 1")},
		{"unfinished without op", "stuck_unfinished_op", stuckInsert("acted",
			"offered, acted_action, acted_at, acted_result", "'', 'regrab', CURRENT_TIMESTAMP, 'unfinished'")},
		{"op id on remove", "stuck_op_regrab", stuckInsert("acted",
			"offered, acted_action, acted_at, acted_result, acted_op_id",
			"'', 'remove', CURRENT_TIMESTAMP, 'ok', 3")},
		{"op id zero", "CHECK constraint failed", stuckInsert("acted",
			"offered, acted_action, acted_at, acted_result, acted_op_id",
			"'', 'regrab', CURRENT_TIMESTAMP, 'ok', 0")},
	}
	for _, c := range cases {
		_, err := raw.ExecContext(context.Background(), c.query)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.constraint) {
			t.Errorf("%s: refused by %v, want %s", c.name, err, c.constraint)
		}
	}
	mustExec(t, raw, `INSERT INTO incidents (id, created_at, updated_at, status, source, reported_by, what, title)
		VALUES ('i', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'open', 'discord', 'x', 'w', 't')`)
	_, err := raw.ExecContext(context.Background(), `UPDATE incidents SET stuck_read_reran = 2`)
	if err == nil || !strings.Contains(err.Error(), "incident_stuck_read_reran") {
		t.Errorf("stuck_read_reran = 2: %v", err)
	}
}

func TestMigration24_OpIDOneWay(t *testing.T) {
	t.Parallel()
	_, raw := migrated24(t)
	const cols = "offered, acted_action, acted_at, acted_result, acted_op_id"
	refused := []string{
		stuckInsert("acted", cols, "'', 'regrab', CURRENT_TIMESTAMP, 'unfinished', NULL"),
		stuckInsert("acted", cols, "'', 'remove', CURRENT_TIMESTAMP, 'ok', 3"),
		stuckInsert("acted", cols+", import_files, import_total",
			"'', 'import', CURRENT_TIMESTAMP, 'ok', 3, 1, 1"),
		stuckInsert("held", "offered, acted_op_id", "'remove', 3"),
		stuckInsert("acted", cols, "'', 'regrab', CURRENT_TIMESTAMP, 'ok', 0"),
		stuckInsert("acted", cols, "'', 'regrab', CURRENT_TIMESTAMP, 'ok', -1"),
		stuckInsert("acted", cols, "'', 'remove', CURRENT_TIMESTAMP, 'unfinished', 3"),
	}
	for _, q := range refused {
		if _, err := raw.ExecContext(context.Background(), q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	accepted := []string{
		"'', 'regrab', CURRENT_TIMESTAMP, 'ok', NULL",
		"'', 'regrab', CURRENT_TIMESTAMP, 'ok', 4",
		"'', 'regrab', CURRENT_TIMESTAMP, 'unfinished', 5",
	}
	for i, vals := range accepted {
		q := strings.Replace(stuckInsert("acted", cols, vals), "'x'", "'ok"+string(rune('a'+i))+"'", 1)
		if _, err := raw.ExecContext(context.Background(), q); err != nil {
			t.Errorf("refused: %s: %v", vals, err)
		}
	}
}

func TestMigration24_StateSetMatchesStates(t *testing.T) {
	t.Parallel()
	_, raw := migrated24(t)
	states := db.StuckStates()
	if len(states) != 7 {
		t.Fatalf("StuckStates = %v, want 7", states)
	}
	insertState := func(s string) error {
		_, err := raw.ExecContext(context.Background(), bareStateInsert, s)
		return err
	}
	for _, s := range states {
		if err := insertState(s); err != nil && strings.Contains(err.Error(), "stuck_state") {
			t.Errorf("state %q refused by stuck_state", s)
		}
	}
	for _, s := range []string{"bogus", "untracked", ""} {
		if err := insertState(s); err == nil || !strings.Contains(err.Error(), "stuck_state") {
			t.Errorf("state %q: %v", s, err)
		}
	}
}

func TestMigration24_RebuildCarriesColumn(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	insert20(t, raw, row20{id: "i1", status: "open", what: "w", item: "j", finding: "f"})
	raw.Close()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw = rawOpen(t, path)
	if n := countInt(t, raw, reranColumns); n != 1 {
		t.Errorf("stuck_read_reran columns: %d", n)
	}
	if n := countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE stuck_read_reran = 0`); n != 1 {
		t.Errorf("seeded incident default: %d", n)
	}
}

const (
	oldUpsertSQL = `INSERT INTO stuck_queue_downloads (app, download_id, title, verdict, first_seen_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(app, download_id) DO UPDATE SET title = excluded.title, verdict = excluded.verdict
			 RETURNING title, verdict, first_seen_at, notified_at`
	oldGetSQL    = `SELECT title, verdict, first_seen_at, notified_at FROM stuck_queue_downloads WHERE app = ? AND download_id = ?`
	oldMarkSQL   = `UPDATE stuck_queue_downloads SET notified_at = ? WHERE app = ? AND download_id = ?`
	oldPruneSQL  = `DELETE FROM stuck_queue_downloads WHERE app = ? AND download_id NOT IN (SELECT value FROM json_each(?))`
	incidentRead = `SELECT id, status, title, updated_at, pending_outcome FROM incidents`
)

func TestMigration24_OldBinarySQLStillRuns(t *testing.T) {
	t.Parallel()
	_, raw := migrated24(t)
	ctx := context.Background()
	var title, verdict string
	var first time.Time
	var notified sql.NullTime
	if err := raw.QueryRowContext(ctx, oldUpsertSQL, "sonarr", "D", "T", "gap", t24()).
		Scan(&title, &verdict, &first, &notified); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := raw.ExecContext(ctx, oldMarkSQL, t24().Add(time.Hour), "sonarr", "D"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	err := raw.QueryRowContext(ctx, oldGetSQL, "sonarr", "D").Scan(&title, &verdict, &first, &notified)
	if err != nil || !notified.Valid {
		t.Fatalf("get: %v %v", notified, err)
	}
	if _, err = raw.ExecContext(ctx, oldPruneSQL, "sonarr", `["other"]`); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := countInt(t, raw, `SELECT COUNT(*) FROM stuck_queue_downloads`); n != 0 {
		t.Errorf("prune left %d", n)
	}
	rows, err := raw.QueryContext(ctx, incidentRead)
	if err != nil {
		t.Fatalf("incident read: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("incident read: %v", err)
	}
}

func fullStuckRow() db.StuckRow {
	return db.StuckRow{
		App: "radarr", DownloadID: "FULL", State: "acted", FirstSeen: t24().Add(-time.Hour),
		NotifiedAt: t24().Add(-30 * time.Minute), TargetsFP: "fp1", ActedAction: "regrab", ActedAt: t24(),
		ActedResult: "unfinished", ActedOpID: 9,
	}
}

func importRow() db.StuckRow {
	return db.StuckRow{
		App: "radarr", DownloadID: "IMP", State: "import_inflight", FirstSeen: t24().Add(-time.Hour),
		NotifiedAt: t24().Add(-30 * time.Minute), TargetsFP: "fp2", ImportCommandID: 41,
		ImportStartedAt: t24(), ImportFiles: 2, ImportTotal: 3,
	}
}

func put(t *testing.T, d *db.DB, app string, rows ...db.StuckRow) {
	t.Helper()
	err := d.ApplyStuck(context.Background(), app, nil, func(map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
		return rows, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStuckRow_RoundTrip(t *testing.T) {
	t.Parallel()
	d, _ := migrated24(t)
	ctx := context.Background()
	for _, want := range []db.StuckRow{fullStuckRow(), importRow(), {
		App: "radarr", DownloadID: "W", State: "waiting", FirstSeen: t24(), Offered: "remove", Misses: 1,
	}} {
		put(t, d, "radarr", want)
		got, ok, err := d.GetStuck(ctx, "radarr", want.DownloadID)
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", want.DownloadID, ok, err)
		}
		if got != want {
			t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
		}
		if got.FirstSeen.Location() != time.UTC {
			t.Errorf("first seen zone %v", got.FirstSeen.Location())
		}
	}
	if _, ok, err := d.GetStuck(ctx, "radarr", "nope"); ok || err != nil {
		t.Errorf("missing: %v %v", ok, err)
	}
	all, err := d.ListStuck(ctx, "radarr")
	if err != nil || len(all) != 3 {
		t.Errorf("ListStuck = %d, %v", len(all), err)
	}
	in, err := d.ListStuckInState(ctx, "acted")
	if err != nil || len(in) != 1 || in[0].DownloadID != "FULL" {
		t.Errorf("ListStuckInState = %+v, %v", in, err)
	}
	before, err := d.ListActedBefore(ctx, t24().Add(time.Second))
	if err != nil || len(before) != 1 {
		t.Errorf("ListActedBefore = %+v, %v", before, err)
	}
	none, err := d.ListActedBefore(ctx, t24())
	if err != nil || len(none) != 0 {
		t.Errorf("ListActedBefore at the instant = %+v, %v", none, err)
	}
}

func TestApplyStuck_DecideErrorWritesNothing(t *testing.T) {
	t.Parallel()
	d, _ := migrated24(t)
	ctx := context.Background()
	put(t, d, "sonarr", db.StuckRow{App: "sonarr", DownloadID: "A", State: "waiting", FirstSeen: t24(),
		Offered: "remove"})
	boom := errors.New("boom")
	err := d.ApplyStuck(ctx, "sonarr", nil, func(map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
		return []db.StuckRow{{App: "sonarr", DownloadID: "B", State: "waiting", FirstSeen: t24(), Offered: "remove"}},
			[]string{"A"}, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	rows, err := d.ListStuck(ctx, "sonarr")
	if err != nil || len(rows) != 1 || rows[0].DownloadID != "A" {
		t.Errorf("rows after failed decide = %+v, %v", rows, err)
	}
}

func TestApplyStuck_LoadsInsideTx(t *testing.T) {
	t.Parallel()
	d, _ := migrated24(t)
	ctx := context.Background()
	mk := func(app, id string) db.StuckRow {
		return db.StuckRow{App: app, DownloadID: id, State: "waiting", FirstSeen: t24(), Offered: "remove"}
	}
	put(t, d, "sonarr", mk("sonarr", "A"), mk("sonarr", "B"))
	put(t, d, "radarr", mk("radarr", "R"))
	var seen []string
	look := func(cur map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
		seen = seen[:0]
		for k, v := range cur {
			if k != v.DownloadID || v.App != "sonarr" {
				t.Errorf("entry %q = %+v", k, v)
			}
			seen = append(seen, k)
		}
		slices.Sort(seen)
		return nil, nil, nil
	}
	if err := d.ApplyStuck(ctx, "sonarr", nil, look); err != nil || !slices.Equal(seen, []string{"A", "B"}) {
		t.Errorf("all: %v %v", seen, err)
	}
	bOnly := []string{"B", "missing"}
	if err := d.ApplyStuck(ctx, "sonarr", bOnly, look); err != nil || !slices.Equal(seen, []string{"B"}) {
		t.Errorf("keys: %v %v", seen, err)
	}
	put(t, d, "sonarr", mk("sonarr", "C"))
	err := d.ApplyStuck(ctx, "sonarr", nil, func(cur map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
		if cur["C"] == nil {
			t.Error("write committed between calls not seen")
		}
		return nil, []string{"C", "ghost"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows, lerr := d.ListStuck(ctx, "sonarr"); lerr != nil || len(rows) != 2 {
		t.Errorf("after delete: %+v %v", rows, lerr)
	}
}
