package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const downChangedIDs = `SELECT id FROM incidents
WHERE status IN ('awaiting_download', 'executing_plan')
   OR (status = 'verifying' AND what <> 'stuck_read')
   OR (status = 'open' AND parked = 1)
   OR (status = 'manual_test_needed' AND reason IN ('plan_interrupted', 'plan_partial'))
ORDER BY id`

const savedDDLObjects = 3

const halfRun = `(status = 'executing_plan' OR reason IN ('plan_interrupted', 'plan_partial'))`

func lifecycleDownJournal() []stmt {
	return []stmt{
		{now: true, sql: `INSERT INTO incident_events (incident_id, at, kind, payload, source)
SELECT id, :now, 'migration_cleared',
       json_object('migration', -21, 'column', 'escalation_plan', 'status', status,
                   'previewed_at', escalation_plan_at, 'value', escalation_plan),
       'migration'
FROM incidents WHERE status = 'executing_plan'`},
		{now: true, sql: `INSERT INTO incident_events (incident_id, at, kind, payload, source)
SELECT id, :now, 'migration_cleared',
       json_object('migration', -21, 'column', 'finding', 'status', status, 'reason', reason, 'value', finding),
       'migration'
FROM incidents
WHERE status = 'executing_plan'
   OR (status = 'manual_test_needed' AND reason IN ('plan_interrupted', 'plan_partial'))`},
		{sql: `CREATE TABLE incidents_stash AS SELECT * FROM incidents`},
		{sql: `DROP TABLE incidents`},
	}
}

const lifecycleDownInsert = `INSERT INTO incidents (
	id, created_at, updated_at, status, source, reported_by, what, title, jellyfin_item_id, details,
	finding, recommended_actions, action_count, autonomous_locked, pending_outcome, pending_outcome_next_check,
	escalation_plan, escalation_plan_at, last_reporter_note_at)
SELECT id, created_at, updated_at,
	CASE status
	     WHEN 'awaiting_download' THEN 'verifying'
	     WHEN 'executing_plan' THEN 'manual_test_needed'
	     WHEN 'verifying' THEN CASE WHEN what = 'stuck_read' THEN 'verifying' ELSE 'reopened' END
	     WHEN 'open' THEN CASE WHEN parked = 1 THEN 'reopened' ELSE 'open' END
	     ELSE status END,
	source, reported_by, what, title, jellyfin_item_id, details,
	CASE WHEN ` + halfRun + ` AND json_valid(finding)
	     THEN json_set(finding, '$.escalate_action', 'manual_investigation') ELSE finding END,
	CASE WHEN ` + halfRun + ` AND json_valid(recommended_actions)
	     THEN json_set(recommended_actions, '$.escalate_action', 'manual_investigation') ELSE recommended_actions END,
	action_count, autonomous_locked,
	pending_outcome,
	CASE WHEN status = 'awaiting_download' THEN verify_next_check END,
	CASE WHEN status = 'executing_plan' THEN NULL ELSE escalation_plan END,
	CASE WHEN status = 'executing_plan' THEN NULL ELSE escalation_plan_at END,
	last_reporter_note_at
FROM incidents_stash`

type downReport struct {
	before, after map[string]int
	changed       []string
	backup        string
}

func MigrateDown(ctx context.Context, path string, version int, out io.Writer) error {
	var run func(context.Context, *sql.Conn, string, io.Writer) error
	switch version {
	case migIncidentLifecycle:
		run = migrateDownOn
	case migOperations:
		run = migrateDown22On
	default:
		return fmt.Errorf("%w: only migrations %d and %d can be rolled back",
			ErrDownRefused, migIncidentLifecycle, migOperations)
	}
	lock, err := takeLock(path, true)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if _, err = os.Stat(path); err != nil {
		return fmt.Errorf("%w: %w", ErrDownRefused, err)
	}
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)"+timeFormatParam)
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(1)
	defer func() { _ = conn.Close() }()
	pinned, err := conn.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = pinned.Close() }()
	return run(ctx, pinned, path, out)
}

func migrateDownOn(ctx context.Context, conn *sql.Conn, path string, out io.Writer) error {
	if err := exclusiveGuard(ctx, conn); err != nil {
		return err
	}
	at, err := downState(ctx, conn)
	if err != nil {
		return err
	}
	if at == migBabysitExplanations {
		counts, cerr := statusCounts(ctx, conn)
		if cerr != nil {
			return cerr
		}
		_, _ = fmt.Fprintf(out, "already at schema 20\n%s", formatCounts("now", counts))
		return nil
	}
	rep := downReport{}
	if rep.before, err = statusCounts(ctx, conn); err != nil {
		return err
	}
	if rep.changed, err = queryIDs(ctx, conn, downChangedIDs); err != nil {
		return err
	}
	if rep.backup, err = backupInto(ctx, conn, path, migIncidentLifecycle); err != nil {
		return err
	}
	if err = rebuildDown(ctx, conn); err != nil {
		return fmt.Errorf("migrate down 21 (backup %s): %w", rep.backup, err)
	}
	if rep.after, err = statusCounts(ctx, conn); err != nil {
		return err
	}
	printDown(out, rep)
	return nil
}

func exclusiveGuard(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		return fmt.Errorf("%w: database in use; stop media-fixer first: %w", ErrDownRefused, err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	var busy, logFrames, checkpointed int
	err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("%w: database in use; stop media-fixer first", ErrDownRefused)
	}
	return nil
}

func downState(ctx context.Context, conn *sql.Conn) (int, error) {
	var maxVersion sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxVersion); err != nil {
		return 0, fmt.Errorf("%w: read schema_version: %w", ErrDownRefused, err)
	}
	var saved, versionCol int
	if err := conn.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_saved_ddl'`,
	).Scan(&saved); err != nil {
		return 0, err
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('incidents') WHERE name = 'version'`).Scan(&versionCol); err != nil {
		return 0, err
	}
	switch {
	case maxVersion.Int64 == migBabysitExplanations && saved == 0 && versionCol == 0:
		return migBabysitExplanations, nil
	case maxVersion.Int64 == migIncidentLifecycle && saved == 1:
		var rows int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_saved_ddl WHERE migration = 21`).Scan(&rows); err != nil {
			return 0, err
		}
		if rows == savedDDLObjects {
			return migIncidentLifecycle, nil
		}
	case maxVersion.Int64 > migIncidentLifecycle:
		return 0, fmt.Errorf("%w: migration %d is applied; 21 must be the newest to roll it back",
			ErrDownRefused, maxVersion.Int64)
	}
	return 0, fmt.Errorf("%w: unexpected schema state (version %d, saved ddl %d, version column %d)",
		ErrDownRefused, maxVersion.Int64, saved, versionCol)
}

func backupInto(ctx context.Context, conn *sql.Conn, path string, version int) (string, error) {
	dest := fmt.Sprintf("%s.pre-down-%d-%s.db", path, version, time.Now().UTC().Format("20060102T150405"))
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("%w: backup %s already exists", ErrDownRefused, dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	return dest, nil
}

func savedDDL(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	var ddl string
	err := tx.QueryRowContext(ctx,
		`SELECT sql FROM schema_saved_ddl WHERE migration = 21 AND name = ?`, name).Scan(&ddl)
	if err != nil {
		return "", fmt.Errorf("saved ddl %s: %w", name, err)
	}
	return ddl, nil
}

func rebuildDown(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	err := runConnTx(ctx, conn, func(ctx context.Context, tx *sql.Tx) error {
		if err := runStatements(ctx, tx, lifecycleDownJournal(), time.Now()); err != nil {
			return err
		}
		ddl, err := savedDDL(ctx, tx, "incidents")
		if err != nil {
			return err
		}
		tail := []stmt{{sql: ddl}, {sql: lifecycleDownInsert}, {sql: `DROP TABLE incidents_stash`}}
		for _, idx := range []string{"idx_incidents_status", "idx_incidents_title"} {
			ix, ierr := savedDDL(ctx, tx, idx)
			if ierr != nil {
				return ierr
			}
			tail = append(tail, stmt{sql: ix})
		}
		tail = append(tail, stmt{sql: `DROP TABLE schema_saved_ddl`},
			stmt{sql: `DELETE FROM schema_version WHERE version = 21`})
		if err = runStatements(ctx, tx, tail, time.Now()); err != nil {
			return err
		}
		return foreignKeyCheck(ctx, tx)
	})
	if _, ferr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys=ON`); ferr != nil {
		err = errors.Join(err, ferr)
	}
	return err
}

func statusCounts(ctx context.Context, conn *sql.Conn) (map[string]int, error) {
	rows, err := conn.QueryContext(ctx, `SELECT status, COUNT(*) FROM incidents GROUP BY status ORDER BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			s string
			n int
		)
		if err = rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

func queryIDs(ctx context.Context, conn *sql.Conn, q string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func formatCounts(label string, counts map[string]int) string {
	total := 0
	var parts []string
	for _, s := range sortedKeys(counts) {
		total += counts[s]
		parts = append(parts, s+"="+strconv.Itoa(counts[s]))
	}
	return fmt.Sprintf("%s: incidents=%d %s\n", label, total, strings.Join(parts, " "))
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func printDown(out io.Writer, r downReport) {
	_, _ = fmt.Fprint(out, formatCounts("before", r.before), formatCounts("after", r.after))
	_, _ = fmt.Fprintf(out, "changed rows: %d\n", len(r.changed))
	if len(r.changed) > 0 {
		_, _ = fmt.Fprintf(out, "changed ids: %s\n", strings.Join(r.changed, " "))
	}
	_, _ = fmt.Fprintf(out, "backup: %s\n", r.backup)
}
