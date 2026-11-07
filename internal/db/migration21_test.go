package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type got21 struct {
	status, reason   string
	po, plan, spec   sql.NullString
	locked           int
	next             sql.NullString
	checksLeft       sql.NullInt64
	version, gen     int
	parked           int
	stage, queueStat sql.NullString
}

func read21(t *testing.T, raw *sql.DB, id string) got21 {
	t.Helper()
	var g got21
	err := raw.QueryRowContext(context.Background(), `SELECT status, COALESCE(reason, ''), pending_outcome,
		escalation_plan, verify_spec, autonomous_locked, verify_next_check, verify_checks_left, version, generation,
		parked, json_extract(pending_outcome, '$.last_stage'), json_extract(pending_outcome, '$.queue_status')
		FROM incidents WHERE id = ?`, id).Scan(&g.status, &g.reason, &g.po, &g.plan, &g.spec, &g.locked, &g.next,
		&g.checksLeft, &g.version, &g.gen, &g.parked, &g.stage, &g.queueStat)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return g
}

func clearedValues(t *testing.T, raw *sql.DB, column string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	eachRow(t, raw, func(rows *sql.Rows) error {
		var id, v string
		err := rows.Scan(&id, &v)
		out[id] = append(out[id], v)
		return err
	}, `SELECT incident_id, json_extract(payload, '$.value')
		FROM incident_events WHERE kind = 'migration_cleared' AND json_extract(payload, '$.column') = ?`, column)
	return out
}

func checkRemap(t *testing.T, r row20, g got21) {
	t.Helper()
	w := expect21(r)
	if g.status != w.status || g.reason != w.reason || g.po.Valid != w.poKept || g.plan.Valid != w.planKept ||
		(g.locked == 1) != w.locked {
		t.Errorf("%s %s po=%v: got %s/%s po=%v plan=%v locked=%d, want %+v", r.id, r.status, r.po != nil, g.status,
			g.reason, g.po.Valid, g.plan.Valid, g.locked, w)
	}
	if w.poKept && (g.stage.String != w.stage || g.queueStat.String != w.queue) {
		t.Errorf("%s: stage %q queue %q, want %q %q", r.id, g.stage.String, g.queueStat.String, w.stage, w.queue)
	}
	if g.spec.Valid != (w.status == "verifying") || g.checksLeft.Valid != (w.status == "verifying") {
		t.Errorf("%s: verify spec %v checks %v in %s", r.id, g.spec.Valid, g.checksLeft.Valid, w.status)
	}
	if g.next.Valid != (w.status == "verifying" || w.status == "awaiting_download") {
		t.Errorf("%s: next check %v in %s", r.id, g.next.Valid, w.status)
	}
	if g.version != 0 || g.gen != 0 || g.parked != 0 {
		t.Errorf("%s: counters %d %d %d", r.id, g.version, g.gen, g.parked)
	}
}

func checkCleared(t *testing.T, r row20, g got21, pos, plans map[string][]string) {
	t.Helper()
	wantPO := r.po != nil && !g.po.Valid
	if wantPO != (len(pos[r.id]) == 1) || (wantPO && pos[r.id][0] != *r.po) {
		t.Errorf("%s: cleared pending outcome journal %v", r.id, pos[r.id])
	}
	wantPlan := r.plan != nil && !g.plan.Valid
	if wantPlan != (len(plans[r.id]) == 1) || (wantPlan && plans[r.id][0] != *r.plan) {
		t.Errorf("%s: cleared plan journal %v", r.id, plans[r.id])
	}
}

func TestMigration21_Synthetic(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	rows := syntheticRows()
	insertAll(t, raw, rows)
	before := childCounts(t, raw)
	d := openDB(t, path)
	ctx := context.Background()
	pos, plans := clearedValues(t, raw, "pending_outcome"), clearedValues(t, raw, "escalation_plan")
	cleared := 0
	for _, r := range rows {
		g := read21(t, raw, r.id)
		checkRemap(t, r, g)
		checkCleared(t, r, g, pos, plans)
		cleared += len(pos[r.id]) + len(plans[r.id])
		snap, err := d.GetIncidentSnapshot(ctx, r.id)
		if err != nil {
			t.Fatalf("%s: %v", r.id, err)
		}
		if err = lifecycle.Check(snap); err != nil {
			t.Errorf("%s: %v", r.id, err)
		}
	}
	after := childCounts(t, raw)
	before[0] += cleared
	if after != before {
		t.Errorf("child counts %v, want %v", after, before)
	}
	assertIntegrity(t, raw)
}

func assertIntegrity(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if n := countInt(t, raw, `SELECT COUNT(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Errorf("foreign_key_check found %d violations", n)
	}
	var ok string
	if err := raw.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Errorf("integrity_check %q %v", ok, err)
	}
	var fk int
	if err := raw.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys %d %v", fk, err)
	}
}

func TestMigration21_RebuildKeepsChildRows(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	insertAll(t, raw, syntheticRows()[:20])
	mustExec(t, raw, `INSERT INTO discord_cards (subject, channel_id, message_id, card, hash, sent_at)
		VALUES ('inc:inc-0001', 'c', 'm', '{}', 'h', ?)`, t20())
	before := childCounts(t, raw)
	openDB(t, path)
	after := childCounts(t, raw)
	if !slices.Equal(after[1:], before[1:]) || after[0] < before[0] {
		t.Fatalf("children %v -> %v", before, after)
	}
}

func TestMigration21_RefusesUnexpectedSourceColumns(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	mustExec(t, raw, `ALTER TABLE incidents ADD COLUMN surprise TEXT`)
	before := master(t, raw)
	if _, err := db.Open(path); err == nil || !strings.Contains(err.Error(), "schema-20 columns") {
		t.Fatalf("Open = %v", err)
	}
	if after := master(t, raw); !slices.Equal(before, after) {
		t.Fatal("a refused migration changed the schema")
	}
	var v int
	if err := raw.QueryRowContext(context.Background(), `SELECT MAX(version) FROM schema_version`).
		Scan(&v); err != nil ||
		v != 20 {
		t.Fatalf("schema version %d %v", v, err)
	}
}

func TestMigration21_ForeignKeyCheckBeforeCommit(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	insert20(t, raw, syntheticRows()[0])
	dangle := func(target *sql.DB) {
		conn, err := target.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx := context.Background()
		if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO incident_reporters (incident_id, reporter, source)
			VALUES ('ghost', 'x', 'discord')`); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
			t.Fatal(err)
		}
	}
	dangle(raw)
	if _, err := db.Open(path); err == nil || !strings.Contains(err.Error(), "foreign_key_check") {
		t.Fatalf("up with a dangling child: %v", err)
	}
	mustExec(t, raw, `DELETE FROM incident_reporters WHERE incident_id = 'ghost'`)
	d := openDB(t, path)
	d.Close()
	dangle(raw)
	raw.Close()
	strip22(t, path)
	var out bytes.Buffer
	if err := db.MigrateDown(context.Background(), path, 21, &out); err == nil ||
		!strings.Contains(err.Error(), "foreign_key_check") {
		t.Fatalf("down with a dangling child: %v", err)
	}
	raw = rawOpen(t, path)
	var v int
	if err := raw.QueryRowContext(context.Background(), `SELECT MAX(version) FROM schema_version`).
		Scan(&v); err != nil ||
		v != 21 {
		t.Fatalf("a refused down changed the version: %d %v", v, err)
	}
}

func TestOpen_ForeignKeysOnForEveryConnection(t *testing.T) {
	t.Parallel()
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	path, raw := schema20(t)
	insert20(t, raw, syntheticRows()[3])
	for _, p := range []string{fresh, path} {
		d := openDB(t, p)
		fks, err := d.ForeignKeysPerConnForTest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(fks, []int{1, 1, 1, 1, 1}) {
			t.Fatalf("%s: foreign_keys per connection %v", p, fks)
		}
	}
}

func TestOpen_HoldsSharedLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "shared.db")
	first := openDB(t, path)
	second := openDB(t, path)
	if _, err := db.LockExclusiveForTest(path); !errors.Is(err, db.ErrDownRefused) {
		t.Fatalf("exclusive lock while open: %v", err)
	}
	info, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode %v", info.Mode().Perm())
	}
	first.Close()
	second.Close()
	release, err := db.LockExclusiveForTest(path)
	if err != nil {
		t.Fatalf("exclusive lock after close: %v", err)
	}
	release()
}

func TestOpen_FailsFastWhileDownHoldsLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "locked.db")
	openDB(t, path).Close()
	release, err := db.LockExclusiveForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = db.Open(path); !errors.Is(err, db.ErrLocked) {
		t.Fatalf("Open while a down holds the lock: %v", err)
	}
}
