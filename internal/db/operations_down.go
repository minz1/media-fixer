package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/operation"
)

const operationsDownJournal = `INSERT INTO incident_events (incident_id, at, kind, payload, source)
SELECT subject_id, :now, 'migration_cleared',
       json_object('migration', -22, 'op_id', id, 'kind', kind, 'status', status,
                   'steps', json(steps), 'results', json(results)),
       'migration'
FROM operations WHERE subject_kind = 'incident' AND status <> 'done'`

func operationsDownStatements() []stmt {
	return []stmt{
		{now: true, sql: operationsDownJournal},
		{sql: `DROP INDEX operations_one_running`},
		{sql: `DROP INDEX operations_subject`},
		{sql: `DROP TABLE operations`},
		{sql: `DELETE FROM schema_version WHERE version = 22`},
	}
}

type operationsDownReport struct {
	byStatus                               map[string]int
	journaled                              int
	resumableIncidents, resumableDownloads int
	backup                                 string
	after                                  int
}

func migrateDown22On(ctx context.Context, conn *sql.Conn, path string, out io.Writer) error {
	if err := exclusiveGuard(ctx, conn); err != nil {
		return err
	}
	at, err := operationsDownState(ctx, conn)
	if err != nil {
		return err
	}
	if at == migIncidentLifecycle {
		counts, cerr := statusCounts(ctx, conn)
		if cerr != nil {
			return cerr
		}
		_, _ = fmt.Fprintf(out, "already at schema 21\n%s", formatCounts("now", counts))
		return nil
	}
	rep, err := operationsDownCounts(ctx, conn)
	if err != nil {
		return err
	}
	if rep.backup, err = backupInto(ctx, conn, path, migOperations); err != nil {
		return err
	}
	err = runConnTx(ctx, conn, func(ctx context.Context, tx *sql.Tx) error {
		return runStatements(ctx, tx, operationsDownStatements(), time.Now())
	})
	if err != nil {
		return fmt.Errorf("migrate down 22 (backup %s): %w", rep.backup, err)
	}
	if err = conn.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&rep.after); err != nil {
		return err
	}
	printOperationsDown(out, rep)
	return nil
}

func operationsDownState(ctx context.Context, conn *sql.Conn) (int, error) {
	var maxVersion sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxVersion); err != nil {
		return 0, fmt.Errorf("%w: read schema_version: %w", ErrDownRefused, err)
	}
	var table int
	if err := conn.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'operations'`,
	).Scan(&table); err != nil {
		return 0, err
	}
	switch {
	case maxVersion.Int64 == migIncidentLifecycle && table == 0:
		return migIncidentLifecycle, nil
	case maxVersion.Int64 == migOperations && table == 1:
		return migOperations, nil
	case maxVersion.Int64 > migOperations:
		return 0, fmt.Errorf("%w: migration %d is applied; 22 must be the newest to roll it back",
			ErrDownRefused, maxVersion.Int64)
	}
	return 0, fmt.Errorf("%w: unexpected schema state (version %d, operations table %d)",
		ErrDownRefused, maxVersion.Int64, table)
}

func operationsDownCounts(ctx context.Context, conn *sql.Conn) (operationsDownReport, error) {
	rep := operationsDownReport{byStatus: map[string]int{}}
	rows, err := conn.QueryContext(ctx, `SELECT status, COUNT(*) FROM operations GROUP BY status`)
	if err != nil {
		return rep, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err = rows.Scan(&status, &n); err != nil {
			return rep, err
		}
		rep.byStatus[status] = n
	}
	if err = rows.Err(); err != nil {
		return rep, err
	}
	open, err := queryOperations(ctx, conn, `status <> 'done'`)
	if err != nil {
		return rep, err
	}
	for _, op := range open {
		if op.Subject.Kind == operation.SubjectIncident {
			rep.journaled++
		}
		if operation.Resumable(op) && op.Subject.Kind == operation.SubjectIncident {
			rep.resumableIncidents++
		} else if operation.Resumable(op) {
			rep.resumableDownloads++
		}
	}
	return rep, nil
}

func printOperationsDown(out io.Writer, r operationsDownReport) {
	var parts []string
	for _, s := range sortedKeys(r.byStatus) {
		parts = append(parts, s+"="+strconv.Itoa(r.byStatus[s]))
	}
	_, _ = fmt.Fprintf(out, "operations dropped: %s\n", strings.Join(parts, " "))
	_, _ = fmt.Fprintf(out, "migration_cleared events: %d\n", r.journaled)
	_, _ = fmt.Fprintf(out, "resumable dropped: incident=%d download=%d\n",
		r.resumableIncidents, r.resumableDownloads)
	_, _ = fmt.Fprintf(out, "schema_version max: %d\n", r.after)
	_, _ = fmt.Fprintf(out, "backup: %s\n", r.backup)
}
