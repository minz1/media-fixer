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

func MigrateUpToForTest(path string, maxVersion int) error {
	ctx := context.Background()
	write, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return err
	}
	defer write.Close()
	write.SetMaxOpenConns(1)
	if _, err = write.ExecContext(ctx, createSchemaVersionTable); err != nil {
		return err
	}
	for _, m := range migrations() {
		if m.version > maxVersion {
			break
		}
		if err = applyMigration(ctx, write, m); err != nil {
			return err
		}
	}
	return nil
}

func LockExclusiveForTest(path string) (func(), error) {
	f, err := takeLock(path, true)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func (d *DB) ForeignKeysPerConnForTest(ctx context.Context) ([]int, error) {
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	pools := []*sql.DB{d.write, d.read, d.read, d.read, d.read}
	var out []int
	for _, p := range pools {
		c, err := p.Conn(ctx)
		if err != nil {
			return nil, err
		}
		conns = append(conns, c)
		var on int
		if err = c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil {
			return nil, err
		}
		out = append(out, on)
	}
	return out, nil
}

const IncidentsLifecycleDDLForTest = incidentsLifecycleDDL

func MigrateTruncatedForTest(path string, maxVersion int) ([]int, error) {
	ctx := context.Background()
	write, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	defer write.Close()
	write.SetMaxOpenConns(1)
	if _, err = write.ExecContext(ctx, createSchemaVersionTable); err != nil {
		return nil, err
	}
	known, err := loadAppliedVersions(ctx, write)
	if err != nil {
		return nil, err
	}
	var ran []int
	for _, m := range migrations() {
		if m.version > maxVersion || known[m.version] {
			continue
		}
		if err = applyMigration(ctx, write, m); err != nil {
			return ran, err
		}
		ran = append(ran, m.version)
	}
	return ran, nil
}

const (
	MigrationRefusalCaseForTest = sweepRefusalCaseFrozen
	RefusalCodeSQLForTest       = refusalCodeSQL
)

func (d *DB) BackfillRefusalCodesForTest(ctx context.Context) error {
	_, err := d.write.ExecContext(ctx, sweepRefusalBackfill)
	return err
}

func (d *DB) QueryIntForTest(ctx context.Context, query string) (int, error) {
	var n int
	err := d.write.QueryRowContext(ctx, query).Scan(&n)
	return n, err
}
