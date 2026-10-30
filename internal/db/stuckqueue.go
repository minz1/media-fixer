package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const stuckQueueSchema = `
CREATE TABLE IF NOT EXISTS stuck_queue_downloads (
	app           TEXT NOT NULL,
	download_id   TEXT NOT NULL,
	title         TEXT NOT NULL,
	verdict       TEXT NOT NULL,
	first_seen_at DATETIME NOT NULL,
	notified_at   DATETIME,
	PRIMARY KEY (app, download_id)
);
`

func stuckQueueMigrations() []migration {
	return []migration{{
		version: migStuckQueue,
		name:    "stuck_queue",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, stuckQueueSchema)
			return err
		},
	}}
}

type StuckDownload struct {
	App         string
	DownloadID  string
	Title       string
	Verdict     string
	FirstSeenAt time.Time
	NotifiedAt  *time.Time
}

func (d *DB) UpsertStuckSeen(
	ctx context.Context,
	app, downloadID, title, verdict string,
	now time.Time,
) (StuckDownload, error) {
	s := StuckDownload{App: app, DownloadID: downloadID}
	var notified sql.NullTime
	err := d.write.QueryRowContext(ctx,
		`INSERT INTO stuck_queue_downloads (app, download_id, title, verdict, first_seen_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(app, download_id) DO UPDATE SET title = excluded.title, verdict = excluded.verdict
		 RETURNING title, verdict, first_seen_at, notified_at`,
		app, downloadID, title, verdict, now).Scan(&s.Title, &s.Verdict, &s.FirstSeenAt, &notified)
	if notified.Valid {
		s.NotifiedAt = &notified.Time
	}
	return s, err
}

func (d *DB) GetStuckSeen(ctx context.Context, app, downloadID string) (StuckDownload, bool, error) {
	s := StuckDownload{App: app, DownloadID: downloadID}
	var notified sql.NullTime
	err := d.read.QueryRowContext(
		ctx,
		`SELECT title, verdict, first_seen_at, notified_at FROM stuck_queue_downloads WHERE app = ? AND download_id = ?`,
		app,
		downloadID,
	).Scan(&s.Title, &s.Verdict, &s.FirstSeenAt, &notified)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	if notified.Valid {
		s.NotifiedAt = &notified.Time
	}
	return s, err == nil, err
}

func (d *DB) MarkStuckNotified(ctx context.Context, app, downloadID string, now time.Time) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE stuck_queue_downloads SET notified_at = ? WHERE app = ? AND download_id = ?`, now, app, downloadID)
	return err
}

func (d *DB) PruneStuckGone(ctx context.Context, app string, keep []string) error {
	if keep == nil {
		keep = []string{}
	}
	ids, err := json.Marshal(keep)
	if err != nil {
		return err
	}
	_, err = d.write.ExecContext(ctx,
		`DELETE FROM stuck_queue_downloads WHERE app = ? AND download_id NOT IN (SELECT value FROM json_each(?))`,
		app, string(ids))
	return err
}
