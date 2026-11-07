package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const migIncidentLifecycle = 21

type stmt struct {
	sql string
	now bool
}

func lifecycleMigrations() []migration {
	return []migration{{
		version: migIncidentLifecycle,
		name:    "incident_lifecycle",
		rebuild: true,
		exec:    migrateIncidentLifecycleUp,
	}}
}

func schema20Columns() []string {
	return []string{
		"id", "created_at", "updated_at", "status", "source", "reported_by", "what", "title", "jellyfin_item_id",
		"details", "finding", "recommended_actions", "action_count", "autonomous_locked", "pending_outcome",
		"pending_outcome_next_check", "escalation_plan", "escalation_plan_at", "last_reporter_note_at",
	}
}

var errSourceShape = errors.New("incidents does not have the schema-20 columns")

func tableColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func runStatements(ctx context.Context, tx *sql.Tx, stmts []stmt, now time.Time) error {
	for i, s := range stmts {
		var args []any
		if s.now {
			args = append(args, sql.Named("now", now))
		}
		if _, err := tx.ExecContext(ctx, s.sql, args...); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
	}
	return nil
}

func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("foreign_key_check found violations; rolled back")
	}
	return rows.Err()
}

func migrateIncidentLifecycleUp(ctx context.Context, tx *sql.Tx) error {
	cols, err := tableColumns(ctx, tx, "incidents")
	if err != nil {
		return err
	}
	if want := schema20Columns(); !slices.Equal(cols, want) {
		return fmt.Errorf("%w: %s", errSourceShape, strings.Join(cols, ","))
	}
	if err = runStatements(ctx, tx, lifecycleUpStatements(), time.Now()); err != nil {
		return err
	}
	return foreignKeyCheck(ctx, tx)
}

func withForeignKeysOff(ctx context.Context, write *sql.DB, fn func(conn *sql.Conn) error) (err error) {
	conn, err := write.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := restoreForeignKeys(ctx, conn); rerr != nil {
			err = errors.Join(err, rerr)
		}
		_ = conn.Close()
	}()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	return fn(conn)
}

func restoreForeignKeys(ctx context.Context, conn *sql.Conn) error {
	ctx = context.WithoutCancel(ctx)
	var on int
	_, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	if err == nil {
		err = conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on)
	}
	if err == nil && on == 1 {
		return nil
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	if err == nil {
		err = errors.New("foreign_keys did not come back on")
	}
	return fmt.Errorf("restore foreign keys: %w", err)
}

func runConnTx(ctx context.Context, conn *sql.Conn, fn func(ctx context.Context, tx *sql.Tx) error) (err error) {
	txn, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = txn.Rollback()
			panic(p)
		}
		if err != nil {
			_ = txn.Rollback()
			return
		}
		err = txn.Commit()
	}()
	return fn(ctx, txn)
}

func applyRebuild(ctx context.Context, write *sql.DB, m migration) error {
	return withForeignKeysOff(ctx, write, func(conn *sql.Conn) error {
		return runConnTx(ctx, conn, func(ctx context.Context, tx *sql.Tx) error {
			if err := m.exec(ctx, tx); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_version (version, name) VALUES (?, ?)`, m.version, m.name)
			return err
		})
	})
}

const incidentsLifecycleDDL = `CREATE TABLE incidents (
	id                         TEXT PRIMARY KEY,
	created_at                 DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at                 DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	status                     TEXT NOT NULL DEFAULT 'open',
	source                     TEXT NOT NULL,
	reported_by                TEXT NOT NULL,
	what                       TEXT NOT NULL,
	title                      TEXT NOT NULL,
	jellyfin_item_id           TEXT,
	details                    TEXT,
	finding                    TEXT,
	recommended_actions        TEXT,
	action_count               INTEGER NOT NULL DEFAULT 0,
	autonomous_locked          INTEGER NOT NULL DEFAULT 0,
	pending_outcome            TEXT,
	pending_outcome_next_check DATETIME,
	escalation_plan            TEXT,
	escalation_plan_at         DATETIME,
	last_reporter_note_at      DATETIME,
	version                    INTEGER NOT NULL DEFAULT 0,
	generation                 INTEGER NOT NULL DEFAULT 0,
	parked                     INTEGER NOT NULL DEFAULT 0,
	reason                     TEXT,
	verify_next_check          DATETIME,
	verify_checks_left         INTEGER,
	verify_spec                TEXT,
	CONSTRAINT incident_status CHECK (status IN ('open', 'reopened', 'investigating', 'verifying',
		'awaiting_download', 'manual_test_needed', 'executing_plan', 'agent_fixed', 'blocked', 'resolved')),
	CONSTRAINT incident_flags CHECK (autonomous_locked IN (0, 1) AND parked IN (0, 1)),
	CONSTRAINT incident_blocked_locked CHECK (status <> 'blocked' OR autonomous_locked = 1),
	CONSTRAINT incident_pending_outcome_owner CHECK (pending_outcome IS NULL
		OR status = 'awaiting_download' OR (status = 'manual_test_needed' AND reason = 'no_release')),
	CONSTRAINT incident_download_has_outcome CHECK (status <> 'awaiting_download' OR pending_outcome IS NOT NULL),
	CONSTRAINT incident_plan_owner CHECK (escalation_plan IS NULL
		OR status IN ('manual_test_needed', 'executing_plan')),
	CONSTRAINT incident_plan_claimed CHECK (status <> 'executing_plan' OR escalation_plan IS NOT NULL),
	CONSTRAINT incident_reason_owner CHECK (reason IS NULL OR status = 'manual_test_needed'),
	CONSTRAINT incident_parked_owner CHECK (parked = 0 OR status IN ('open', 'reopened')),
	CONSTRAINT incident_counters CHECK (version >= 0 AND generation >= 0 AND action_count >= 0),
	CONSTRAINT incident_verify_schedule CHECK (status NOT IN ('verifying', 'awaiting_download')
		OR verify_next_check IS NOT NULL),
	CONSTRAINT incident_verify_spec CHECK (verify_spec IS NULL
		OR (status = 'verifying' AND json_valid(verify_spec)))
)`

const poStageNormal = `CASE WHEN COALESCE(json_extract(pending_outcome, '$.last_stage'), '') IN ('', 'searching')`

func lifecycleUpStatements() []stmt {
	return []stmt{
		{sql: `CREATE TABLE schema_saved_ddl (migration INTEGER NOT NULL, type TEXT NOT NULL, name TEXT NOT NULL,
	sql TEXT NOT NULL, PRIMARY KEY (migration, name))`},
		{sql: `INSERT INTO schema_saved_ddl (migration, type, name, sql)
SELECT 21, type, name, sql FROM sqlite_master
WHERE name IN ('incidents', 'idx_incidents_status', 'idx_incidents_title')`},
		{sql: `CREATE TABLE incidents_stash AS
SELECT *,
	CASE WHEN status = 'verifying' AND po_class IN ('tracked', 'no_release') THEN 'awaiting_download'
	     ELSE status END AS new_status,
	CASE WHEN status = 'manual_test_needed' THEN
	     CASE po_class WHEN 'none' THEN 'needs_owner' WHEN 'no_release' THEN 'no_release'
	          ELSE 'download_problem' END END AS new_reason,
	CASE WHEN po_class = 'no_release' OR (status = 'verifying' AND po_class = 'tracked') THEN 1
	     ELSE 0 END AS po_keep
FROM (SELECT *, CASE
	WHEN pending_outcome IS NULL THEN 'none'
	WHEN NOT json_valid(pending_outcome) THEN 'bad'
	WHEN status = 'manual_test_needed'
	 AND COALESCE(json_extract(pending_outcome, '$.grab_notified'), 0) = 0
	 AND COALESCE(json_extract(pending_outcome, '$.last_stage'), '') IN ('', 'searching')
	 AND NOT COALESCE((julianday((SELECT MAX(e.at) FROM incident_events e
	          WHERE e.incident_id = incidents.id AND e.kind = 'status_changed' AND json_valid(e.payload)
	            AND json_extract(e.payload, '$.status') = 'manual_test_needed'))
	        - julianday(json_extract(pending_outcome, '$.started_at'))) * 24
	      > COALESCE((SELECT json_extract(value, '$.track_cap_hours') FROM settings
	          WHERE key = 'maintenance.incident_timers' AND json_valid(value)), 12), 0)
	 THEN 'no_release'
	ELSE 'tracked' END AS po_class FROM incidents)`},
		{now: true, sql: `INSERT INTO incident_events (incident_id, at, kind, payload, source)
SELECT id, :now, 'migration_cleared',
       json_object('migration', 21, 'column', 'pending_outcome', 'status', status,
                   'next_check', pending_outcome_next_check, 'value', pending_outcome),
       'migration'
FROM incidents_stash WHERE pending_outcome IS NOT NULL AND po_keep = 0`},
		{now: true, sql: `INSERT INTO incident_events (incident_id, at, kind, payload, source)
SELECT id, :now, 'migration_cleared',
       json_object('migration', 21, 'column', 'escalation_plan', 'status', status,
                   'previewed_at', escalation_plan_at, 'value', escalation_plan),
       'migration'
FROM incidents_stash WHERE escalation_plan IS NOT NULL AND status <> 'manual_test_needed'`},
		{sql: `DROP TABLE incidents`},
		{sql: incidentsLifecycleDDL},
		{now: true, sql: `INSERT INTO incidents (
	id, created_at, updated_at, status, source, reported_by, what, title, jellyfin_item_id, details,
	finding, recommended_actions, action_count, autonomous_locked, pending_outcome, pending_outcome_next_check,
	escalation_plan, escalation_plan_at, last_reporter_note_at,
	version, generation, parked, reason, verify_next_check, verify_checks_left, verify_spec)
SELECT
	id, created_at, updated_at, new_status, source, reported_by, what, title, jellyfin_item_id, details,
	finding, recommended_actions, action_count,
	CASE WHEN status = 'blocked' THEN 1 ELSE autonomous_locked END,
	CASE WHEN po_keep = 1 THEN json_set(pending_outcome,
	     '$.last_stage', ` + poStageNormal + ` THEN 'searching' ELSE 'downloading' END,
	     '$.queue_status', ` + poStageNormal + ` THEN '' ELSE json_extract(pending_outcome, '$.last_stage') END)
	END,
	NULL,
	CASE WHEN status = 'manual_test_needed' THEN escalation_plan END,
	CASE WHEN status = 'manual_test_needed' THEN escalation_plan_at END,
	last_reporter_note_at,
	0, 0, 0,
	new_reason,
	CASE WHEN new_status = 'awaiting_download' THEN COALESCE(pending_outcome_next_check, :now)
	     WHEN new_status = 'verifying' THEN :now END,
	CASE WHEN new_status = 'verifying' THEN 1 END,
	CASE WHEN new_status = 'verifying' THEN json_object(
	     'kind', CASE WHEN what = 'stuck_read' THEN 'stuck_read' ELSE 'generic' END,
	     'item_id', COALESCE(jellyfin_item_id, ''), 'extensions', 0, 'eta_noted', json('false')) END
FROM incidents_stash`},
		{sql: `DROP TABLE incidents_stash`},
		{sql: `CREATE INDEX idx_incidents_status ON incidents(status)`},
		{sql: `CREATE INDEX idx_incidents_title  ON incidents(title)`},
	}
}
