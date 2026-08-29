package db

import (
	"context"
	"database/sql"
)

// JournalModeForTest returns the write pool's current journal_mode pragma
// value, for internal/db/db_test.go (package db_test).
func (d *DB) JournalModeForTest(ctx context.Context) (string, error) {
	var v string
	err := d.write.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&v)
	return v, err
}

// ForeignKeysEnabledForTest reports whether the write pool has foreign key
// enforcement on.
func (d *DB) ForeignKeysEnabledForTest(ctx context.Context) (bool, error) {
	var v int
	err := d.write.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&v)
	return v == 1, err
}

// SchemaVersionsForTest returns every version recorded in schema_version, in
// ascending order.
func (d *DB) SchemaVersionsForTest(ctx context.Context) ([]int, error) {
	rows, err := d.write.QueryContext(ctx, `SELECT version FROM schema_version ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var v int
		if scanErr := rows.Scan(&v); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ExecForTest runs a raw statement against the write pool, for tests that
// need to poke at the schema directly (e.g. seeding an orphaned row to
// verify foreign key enforcement, or a legacy pre-schema_version fixture).
func (d *DB) ExecForTest(ctx context.Context, query string, args ...any) error {
	_, err := d.write.ExecContext(ctx, query, args...)
	return err
}

// LegacySchemaForTest, LegacyDedupReportersByDiscordIDForTest, and
// LegacyCreateReporterDiscordIndexForTest expose the exact SQL the
// pre-schema_version Open() used to run unconditionally on every startup,
// for tests that build a fixture simulating the real production database
// as it existed before migrations were versioned (see
// TestOpen_BootstrapsLegacyDatabase in db_test.go).
const (
	LegacySchemaForTest                     = schema
	LegacyDedupReportersByDiscordIDForTest  = dedupReportersByDiscordID
	LegacyCreateReporterDiscordIndexForTest = createReporterDiscordIndex
)

// BeginWriteTxForTest starts a raw transaction on the write pool, for tests
// proving the read pool isn't blocked by an in-flight write transaction —
// the reason this DB has two pools instead of one.
func (d *DB) BeginWriteTxForTest(ctx context.Context) (*sql.Tx, error) {
	return d.write.BeginTx(ctx, nil)
}

// PingReadForTest issues a trivial query against the read pool.
func (d *DB) PingReadForTest(ctx context.Context) error {
	var one int
	return d.read.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}
