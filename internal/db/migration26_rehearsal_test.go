package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
)

func TestMigration26_RehearsalOnSnapshot(t *testing.T) {
	t.Parallel()
	snap := os.Getenv("MF_SNAPSHOT")
	if snap == "" {
		t.Skip("MF_SNAPSHOT is not set")
	}
	ctx := context.Background()
	path := copySnapshot(t, snap)
	if _, err := db.MigrateTruncatedForTest(path, 25); err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, path)
	before := tableCounts(t, raw)
	t.Logf("counts at 25: %v", before)
	raw.Close()

	d := openDB(t, path)
	raw = rawOpen(t, path)
	after := tableCounts(t, raw)
	if after["notify_outbox"] != 0 {
		t.Errorf("notify_outbox has %d rows", after["notify_outbox"])
	}
	delete(after, "notify_outbox")
	delete(after, "schema_version")
	delete(before, "schema_version")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("counts changed on up: %v -> %v", before, after)
	}
	integrityOK(t, raw)
	observed, skipped, err := d.ListDriven(ctx)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("ListDriven on the migrated DB: %d skipped, %v", len(skipped), err)
	}
	ids := strs(t, raw, `SELECT id FROM incidents`)
	for _, id := range ids {
		if _, err = d.GetIncidentSnapshot(ctx, id); err != nil {
			t.Fatalf("snapshot %s on the migrated DB: %v", id, err)
		}
	}
	t.Logf("driven incidents %d, snapshots read %d", len(observed), len(ids))
	d.Close()
	raw.Close()

	var out bytes.Buffer
	if err = db.MigrateDown(ctx, path, 26, &out); err != nil {
		t.Fatal(err)
	}
	t.Logf("down: %s", out.String())
	raw = rawOpen(t, path)
	down := tableCounts(t, raw)
	delete(down, "schema_version")
	if !reflect.DeepEqual(before, down) {
		t.Errorf("counts changed on down: %v -> %v", before, down)
	}
	integrityOK(t, raw)
	if v := maxVersion(t, raw); v != 25 {
		t.Fatalf("version %d after down", v)
	}
	raw.Close()
	openDB(t, path)
	raw = rawOpen(t, path)
	if v := maxVersion(t, raw); v != 26 {
		t.Fatalf("version %d after re-up", v)
	}
	integrityOK(t, raw)
}

func integrityOK(t *testing.T, raw interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) {
	t.Helper()
	var s string
	if err := raw.QueryRowContext(context.Background(), `PRAGMA integrity_check`).Scan(&s); err != nil || s != "ok" {
		t.Fatalf("integrity_check %q %v", s, err)
	}
}
