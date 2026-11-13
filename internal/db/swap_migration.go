package db

import (
	"context"
	"database/sql"
	"strings"
)

const migSwapRelease = 27

func operationsSwapDDL() string {
	return strings.Replace(operationsTableDDL, `'regrab')),`, `'regrab', 'swap_release', 'resync_subtitle')),`, 1)
}

func rebuildOperations(ctx context.Context, tx *sql.Tx, ddl string) error {
	for _, q := range []string{
		`CREATE TABLE operations_seq AS SELECT seq FROM sqlite_sequence WHERE name = 'operations'`,
		`CREATE TABLE operations_stash AS SELECT * FROM operations`,
		`DROP TABLE operations`,
		ddl,
		`INSERT INTO operations SELECT * FROM operations_stash`,
		`DELETE FROM sqlite_sequence WHERE name = 'operations'`,
		`INSERT INTO sqlite_sequence (name, seq) SELECT 'operations', seq FROM operations_seq`,
		`DROP TABLE operations_stash`,
		`DROP TABLE operations_seq`,
		operationsSubjectIndexDDL,
		operationsOneRunningDDL,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func swapReleaseMigrations() []migration {
	return []migration{{
		version: migSwapRelease,
		name:    "swap_release_operations",
		rebuild: true,
		exec: func(ctx context.Context, tx *sql.Tx) error {
			return rebuildOperations(ctx, tx, operationsSwapDDL())
		},
	}}
}
