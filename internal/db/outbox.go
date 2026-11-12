package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"time"
)

const migNotifyOutbox = 26

const notifyOutboxSchema = `
CREATE TABLE notify_outbox (
	id          INTEGER PRIMARY KEY,
	key         TEXT NOT NULL UNIQUE,
	incident_id TEXT,
	target      TEXT NOT NULL,
	kind        TEXT NOT NULL,
	body        TEXT NOT NULL,
	created_at  DATETIME NOT NULL,
	next_at     DATETIME NOT NULL,
	attempts    INTEGER NOT NULL DEFAULT 0,
	sent_at     DATETIME,
	gave_up_at  DATETIME,
	last_error  TEXT
);
CREATE INDEX notify_outbox_due ON notify_outbox(next_at) WHERE sent_at IS NULL AND gave_up_at IS NULL;
ALTER TABLE incidents ADD COLUMN last_seerr_note_at DATETIME;`

func outboxMigrations() []migration {
	return []migration{{
		version: migNotifyOutbox,
		name:    "notify_outbox",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, notifyOutboxSchema)
			return err
		},
	}}
}

type OutboxRow struct {
	ID                                  int64
	Key, IncidentID, Target, Kind, Body string
	CreatedAt, NextAt                   time.Time
	Attempts                            int
}

func enqueueOutbox(ctx context.Context, tx *sql.Tx, r OutboxRow) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notify_outbox
		(key, incident_id, target, kind, body, created_at, next_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.Key, nullString(r.IncidentID), r.Target, r.Kind, r.Body, r.CreatedAt, r.NextAt)
	return err
}

func (d *DB) EnqueueOutbox(ctx context.Context, r OutboxRow) error {
	return runTx(ctx, d.write, func(ctx context.Context, tx *sql.Tx) error { return enqueueOutbox(ctx, tx, r) })
}

func (d *DB) DueOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxRow, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT id, key, COALESCE(incident_id, ''), target, kind, body,
		created_at, next_at, attempts FROM notify_outbox o
		WHERE sent_at IS NULL AND gave_up_at IS NULL AND next_at <= :now
		  AND NOT EXISTS (SELECT 1 FROM notify_outbox p WHERE p.target = o.target AND p.id < o.id
		    AND p.sent_at IS NULL AND p.gave_up_at IS NULL AND p.next_at > :now)
		ORDER BY id LIMIT :limit`, sql.Named("now", now), sql.Named("limit", limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err = rows.Scan(&r.ID, &r.Key, &r.IncidentID, &r.Target, &r.Kind, &r.Body,
			&r.CreatedAt, &r.NextAt, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) MarkOutboxSent(ctx context.Context, id int64, at time.Time) error {
	return d.execOutbox(ctx, `UPDATE notify_outbox SET sent_at = ?, attempts = attempts + 1 WHERE id = ?`, at, id)
}

func (d *DB) MarkOutboxFailed(ctx context.Context, id int64, next time.Time, errText string) error {
	return d.execOutbox(ctx,
		`UPDATE notify_outbox SET next_at = ?, attempts = attempts + 1, last_error = ? WHERE id = ?`, next, errText, id)
}

func (d *DB) GiveUpOutbox(ctx context.Context, id int64, at time.Time) error {
	return d.execOutbox(ctx, `UPDATE notify_outbox SET gave_up_at = ? WHERE id = ?`, at, id)
}

func (d *DB) StuckOutboxCount(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := d.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM notify_outbox WHERE gave_up_at >= ?`,
		now.Add(-7*24*time.Hour)).Scan(&n)
	return n, err
}

func (d *DB) execOutbox(ctx context.Context, q string, args ...any) error {
	_, err := d.write.ExecContext(ctx, q, args...)
	return err
}

func migrateDown26On(ctx context.Context, conn *sql.Conn, path string, out io.Writer) error {
	if err := exclusiveGuard(ctx, conn); err != nil {
		return err
	}
	var maxVersion sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&maxVersion); err != nil {
		return fmt.Errorf("%w: read schema_version: %w", ErrDownRefused, err)
	}
	switch {
	case maxVersion.Int64 == migSweepRefusalCode:
		_, _ = fmt.Fprintln(out, "already at schema 25")
		return nil
	case maxVersion.Int64 != migNotifyOutbox:
		return fmt.Errorf("%w: migration %d is applied; 26 must be the newest to roll it back",
			ErrDownRefused, maxVersion.Int64)
	}
	var total, pending int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(sent_at IS NULL AND gave_up_at IS NULL), 0)
		FROM notify_outbox`).Scan(&total, &pending); err != nil {
		return err
	}
	backup, err := backupInto(ctx, conn, path, migNotifyOutbox)
	if err != nil {
		return err
	}
	err = runConnTx(ctx, conn, func(ctx context.Context, tx *sql.Tx) error {
		for _, q := range []string{
			`DROP TABLE notify_outbox`,
			`ALTER TABLE incidents DROP COLUMN last_seerr_note_at`,
			`DELETE FROM schema_version WHERE version = 26`,
		} {
			if _, xerr := tx.ExecContext(ctx, q); xerr != nil {
				return xerr
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("migrate down 26 (backup %s): %w", backup, err)
	}
	_, _ = fmt.Fprintf(out, "outbox rows dropped: %d (undelivered %d)\nschema_version max: 25\nbackup: %s\n",
		total, pending, backup)
	return nil
}
