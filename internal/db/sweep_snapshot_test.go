package db_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func TestMigration25_SweepOnSnapshot(t *testing.T) {
	t.Parallel()
	snap := os.Getenv("MF_SNAPSHOT")
	if snap == "" {
		t.Skip("MF_SNAPSHOT is not set")
	}
	path := copySnapshot(t, snap)
	before := readSweepCounts(t, path)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	versions, err := d.SchemaVersionsForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(versions, 25) {
		t.Errorf("versions = %v, want 25 recorded", versions)
	}
	if n := rawCount(t, d, `SELECT COUNT(*) FROM pragma_table_info('sweep_refused') WHERE name = 'code'`); n != 1 {
		t.Errorf("code column count = %d, want 1", n)
	}
	after := readSweepCounts(t, path)
	t.Logf("sweep counts before: %s", before.table())
	t.Logf("sweep counts after:  %s", after.table())
	if before.table() != after.table() {
		t.Errorf("counts changed: before %s, after %s", before.table(), after.table())
	}
	if before.settingsHash != after.settingsHash {
		t.Errorf("maintenance.pack_sweep hash changed: %s -> %s", before.settingsHash, after.settingsHash)
	}
	checkSnapshotMachines(t, d, path)
	checkSnapshotSettings(t, d, path)
}

func checkSnapshotSettings(t *testing.T, d *db.DB, path string) {
	t.Helper()
	s, err := sweep.LoadSettings(context.Background(), d)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if !s.Enabled || s.IntervalHours != 168 {
		t.Errorf("settings enabled = %v, interval = %d; want true, 168", s.Enabled, s.IntervalHours)
	}
	var raw string
	row := openReadOnly(t, path).QueryRow(`SELECT value FROM settings WHERE key = 'maintenance.pack_sweep'`)
	if err = row.Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		EnabledAt *time.Time `json:"enabled_at"`
	}
	if err = json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	switch {
	case stored.EnabledAt == nil && s.EnabledAt != nil:
		t.Errorf("EnabledAt = %v, stored has none", s.EnabledAt)
	case stored.EnabledAt != nil && (s.EnabledAt == nil || !s.EnabledAt.Equal(*stored.EnabledAt)):
		t.Errorf("EnabledAt = %v, stored %v", s.EnabledAt, stored.EnabledAt)
	}
}

type rawRun struct{ hasCounts, hasFinished bool }

func readRawRuns(t *testing.T, path string) map[int64]rawRun {
	t.Helper()
	rows, err := openReadOnly(t, path).Query(
		`SELECT id, counts IS NOT NULL, finished_at IS NOT NULL FROM sweep_runs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]rawRun{}
	for rows.Next() {
		var id int64
		var r rawRun
		if err = rows.Scan(&id, &r.hasCounts, &r.hasFinished); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkSnapshotMachines(t *testing.T, d *db.DB, path string) {
	t.Helper()
	raws := readRawRuns(t, path)
	runs, err := d.ListSweepRuns(context.Background(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runs {
		run := &runs[i]
		snap := sweepstate.RunSnap{
			Status: run.Status, HasCounts: raws[run.ID].hasCounts, HasFinished: raws[run.ID].hasFinished,
			Reason: run.Reason,
		}
		if err = sweepstate.CheckRun(snap); err != nil {
			t.Errorf("run %d: %v", run.ID, err)
		}
		for _, it := range run.Items {
			if err = sweepstate.CheckItem(it.Result, run.Dry, it.ReleaseTitle, it.Episodes); err != nil {
				t.Errorf("run %d item %d: %v", run.ID, it.ID, err)
			}
		}
	}
}

func openReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func copySnapshotFile(t *testing.T, src, dst string) bool {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
	return true
}

func copySnapshot(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if !copySnapshotFile(t, src, dst) {
		t.Fatalf("snapshot %s not found", src)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		copySnapshotFile(t, src+suffix, dst+suffix)
	}
	return dst
}

type sweepCounts struct {
	runs, items      map[string]int
	refused, targets int
	settingsHash     string
}

func (c sweepCounts) table() string {
	return fmt.Sprintf("runs=%v items=%v refused=%d targets=%d", c.runs, c.items, c.refused, c.targets)
}

func groupCounts(t *testing.T, conn *sql.DB, query string) map[string]int {
	t.Helper()
	rows, err := conn.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err = rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out[k] = n
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func readSweepCounts(t *testing.T, path string) sweepCounts {
	t.Helper()
	conn := openReadOnly(t, path)
	c := sweepCounts{
		runs:  groupCounts(t, conn, `SELECT status, COUNT(*) FROM sweep_runs GROUP BY status`),
		items: groupCounts(t, conn, `SELECT result, COUNT(*) FROM sweep_items GROUP BY result`),
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sweep_refused`).Scan(&c.refused); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sweep_targets`).Scan(&c.targets); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := conn.QueryRow(`SELECT value FROM settings WHERE key = 'maintenance.pack_sweep'`).
		Scan(&value); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(value))
	c.settingsHash = hex.EncodeToString(sum[:])
	return c
}
