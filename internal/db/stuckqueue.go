package db

import (
	"context"
	"database/sql"
	"encoding/json"
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

func stuckDownloadsMigrations() []migration {
	return []migration{{
		version: migStuckDownloads,
		name:    "stuck_downloads",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, stuckDownloadsSchema)
			return err
		},
	}}
}

const stuckDownloadsSchema = `
CREATE TABLE stuck_downloads (
	app               TEXT NOT NULL,
	download_id       TEXT NOT NULL,
	state             TEXT NOT NULL,
	first_seen        DATETIME NOT NULL,
	notified_at       DATETIME,
	misses            INTEGER NOT NULL DEFAULT 0,
	targets_fp        TEXT NOT NULL DEFAULT '',
	offered           TEXT NOT NULL DEFAULT '',
	acted_action      TEXT,
	acted_at          DATETIME,
	acted_result      TEXT,
	acted_op_id       INTEGER CHECK (acted_op_id IS NULL OR acted_op_id > 0),
	import_command_id INTEGER,
	import_started_at DATETIME,
	import_files      INTEGER,
	import_total      INTEGER,
	PRIMARY KEY (app, download_id),
	CONSTRAINT stuck_app CHECK (app IN ('sonarr', 'radarr')),
	CONSTRAINT stuck_state CHECK (state IN
		('waiting', 'due', 'held', 'notified', 'import_inflight', 'acted', 'cleared')),
	CONSTRAINT stuck_misses CHECK (misses IN (0, 1)
		AND (misses = 0 OR state NOT IN ('import_inflight', 'acted'))),
	CONSTRAINT stuck_offered CHECK (offered IN ('', 'remove', 'regrab remove', 'import regrab remove')
		AND (state <> 'waiting' OR offered = 'remove')
		AND (state NOT IN ('import_inflight', 'acted') OR offered = '')),
	CONSTRAINT stuck_notified CHECK ((state <> 'notified' OR notified_at IS NOT NULL)
		AND (state <> 'due' OR notified_at IS NULL)),
	CONSTRAINT stuck_import CHECK ((state = 'import_inflight') = (import_command_id IS NOT NULL)
		AND (import_command_id IS NULL) = (import_started_at IS NULL)
		AND (import_command_id IS NULL OR import_command_id > 0)),
	CONSTRAINT stuck_import_files CHECK ((import_files IS NULL) = (import_total IS NULL)
		AND (import_files IS NULL OR (import_files > 0 AND import_files <= import_total))
		AND (import_files IS NOT NULL) = (state = 'import_inflight' OR acted_action IS 'import')),
	CONSTRAINT stuck_acted_whole CHECK ((acted_action IS NULL) = (acted_at IS NULL)
		AND (acted_at IS NULL) = (acted_result IS NULL)),
	CONSTRAINT stuck_acted_values CHECK (acted_action IS NULL OR (acted_action IN ('import', 'regrab', 'remove')
		AND acted_result IN ('ok', 'unfinished', 'import_failed', 'import_timeout', 'import_unconfirmed',
			'partial'))),
	CONSTRAINT stuck_acted_owner CHECK (CASE state
		WHEN 'acted' THEN acted_result IS NOT NULL AND acted_result IN ('ok', 'unfinished')
		WHEN 'notified' THEN acted_result IS NULL OR (acted_action = 'import'
			AND acted_result IN ('import_failed', 'import_timeout', 'import_unconfirmed', 'partial'))
		ELSE acted_action IS NULL END),
	CONSTRAINT stuck_unfinished_regrab CHECK (acted_result IS NOT 'unfinished' OR acted_action IS 'regrab'),
	CONSTRAINT stuck_unfinished_op CHECK (acted_result IS NOT 'unfinished' OR acted_op_id IS NOT NULL),
	CONSTRAINT stuck_op_regrab CHECK (acted_op_id IS NULL OR acted_action IS 'regrab')
);

INSERT INTO stuck_downloads (app, download_id, state, first_seen, notified_at, misses, targets_fp, offered)
SELECT app, download_id,
       CASE WHEN notified_at IS NULL THEN 'waiting' ELSE 'notified' END,
       first_seen_at, notified_at, 0, '',
       CASE WHEN notified_at IS NULL THEN 'remove' ELSE '' END
FROM stuck_queue_downloads
WHERE app IN ('sonarr', 'radarr')
ON CONFLICT(app, download_id) DO NOTHING;

ALTER TABLE incidents ADD COLUMN stuck_read_reran INTEGER NOT NULL DEFAULT 0
	CONSTRAINT incident_stuck_read_reran CHECK (stuck_read_reran IN (0, 1));
`

type StuckRow struct {
	App             string
	DownloadID      string
	State           string
	FirstSeen       time.Time
	NotifiedAt      time.Time
	Misses          int
	TargetsFP       string
	Offered         string
	ActedAction     string
	ActedAt         time.Time
	ActedResult     string
	ActedOpID       int64
	ImportCommandID int
	ImportStartedAt time.Time
	ImportFiles     int
	ImportTotal     int
}

func StuckStates() []string {
	return []string{"waiting", "due", "held", "notified", "import_inflight", "acted", "cleared"}
}

const stuckColumns = `app, download_id, state, first_seen, notified_at, misses, targets_fp, offered,
	acted_action, acted_at, acted_result, acted_op_id, import_command_id, import_started_at, import_files, import_total`

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func timeOf(n sql.NullTime) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return n.Time.UTC()
}

func scanStuck(rows *sql.Rows) (StuckRow, error) {
	var r StuckRow
	var notified, actedAt, importAt sql.NullTime
	var action, result sql.NullString
	var op, cmd, files, total sql.NullInt64
	err := rows.Scan(&r.App, &r.DownloadID, &r.State, &r.FirstSeen, &notified, &r.Misses, &r.TargetsFP, &r.Offered,
		&action, &actedAt, &result, &op, &cmd, &importAt, &files, &total)
	if err != nil {
		return StuckRow{}, err
	}
	r.FirstSeen = r.FirstSeen.UTC()
	r.NotifiedAt = timeOf(notified)
	r.ActedAction = action.String
	r.ActedAt = timeOf(actedAt)
	r.ActedResult = result.String
	r.ActedOpID = op.Int64
	r.ImportCommandID = int(cmd.Int64)
	r.ImportStartedAt = timeOf(importAt)
	r.ImportFiles = int(files.Int64)
	r.ImportTotal = int(total.Int64)
	return r, nil
}

func queryStuck(ctx context.Context, q opQuerier, where string, args ...any) ([]StuckRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+stuckColumns+` FROM stuck_downloads WHERE `+where+
		` ORDER BY app, download_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StuckRow
	for rows.Next() {
		r, scanErr := scanStuck(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) ListStuck(ctx context.Context, app string) ([]StuckRow, error) {
	return queryStuck(ctx, d.read, `app = ?`, app)
}

func (d *DB) GetStuck(ctx context.Context, app, downloadID string) (StuckRow, bool, error) {
	rows, err := queryStuck(ctx, d.read, `app = ? AND download_id = ?`, app, downloadID)
	if err != nil || len(rows) == 0 {
		return StuckRow{}, false, err
	}
	return rows[0], true, nil
}

func (d *DB) ListStuckInState(ctx context.Context, state string) ([]StuckRow, error) {
	return queryStuck(ctx, d.read, `state = ?`, state)
}

func (d *DB) ListActedBefore(ctx context.Context, t time.Time) ([]StuckRow, error) {
	return queryStuck(ctx, d.read, `state = 'acted' AND acted_at < ?`, t.UTC())
}

type StuckStep struct {
	App    string
	Keys   []string
	Decide func(cur map[string]*StuckRow) ([]StuckRow, []string, error)
}

func (d *DB) ApplyStuck(
	ctx context.Context,
	app string,
	keys []string,
	decide func(cur map[string]*StuckRow) ([]StuckRow, []string, error),
) error {
	return d.ApplyStuckBatch(ctx, []StuckStep{{App: app, Keys: keys, Decide: decide}}, nil)
}

func (d *DB) ApplyStuckBatch(ctx context.Context, steps []StuckStep, outbox func() []OutboxRow) error {
	return runTx(ctx, d.write, func(ctx context.Context, tx *sql.Tx) error {
		for _, s := range steps {
			if err := applyStuckStep(ctx, tx, s); err != nil {
				return err
			}
		}
		if outbox == nil {
			return nil
		}
		for _, r := range outbox() {
			if err := enqueueOutbox(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

func applyStuckStep(ctx context.Context, tx *sql.Tx, s StuckStep) error {
	rows, err := loadStuck(ctx, tx, s.App, s.Keys)
	if err != nil {
		return err
	}
	cur := make(map[string]*StuckRow, len(rows))
	for i := range rows {
		cur[rows[i].DownloadID] = &rows[i]
	}
	put, del, err := s.Decide(cur)
	if err != nil {
		return err
	}
	for _, id := range del {
		if err = deleteStuck(ctx, tx, s.App, id); err != nil {
			return err
		}
	}
	for _, r := range put {
		if err = upsertStuck(ctx, tx, r); err != nil {
			return err
		}
	}
	return nil
}

func loadStuck(ctx context.Context, tx *sql.Tx, app string, keys []string) ([]StuckRow, error) {
	if keys == nil {
		return queryStuck(ctx, tx, `app = ?`, app)
	}
	ids, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	return queryStuck(ctx, tx, `app = ? AND download_id IN (SELECT value FROM json_each(?))`, app, string(ids))
}

func deleteStuck(ctx context.Context, tx *sql.Tx, app, downloadID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM stuck_downloads WHERE app = ? AND download_id = ?`, app, downloadID)
	return err
}

func upsertStuck(ctx context.Context, tx *sql.Tx, r StuckRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO stuck_downloads (`+stuckColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app, download_id) DO UPDATE SET state = excluded.state, first_seen = excluded.first_seen,
			notified_at = excluded.notified_at, misses = excluded.misses, targets_fp = excluded.targets_fp,
			offered = excluded.offered, acted_action = excluded.acted_action, acted_at = excluded.acted_at,
			acted_result = excluded.acted_result, acted_op_id = excluded.acted_op_id,
			import_command_id = excluded.import_command_id, import_started_at = excluded.import_started_at,
			import_files = excluded.import_files, import_total = excluded.import_total`,
		r.App, r.DownloadID, r.State, r.FirstSeen.UTC(), nullTime(r.NotifiedAt), r.Misses, r.TargetsFP, r.Offered,
		nullString(r.ActedAction), nullTime(r.ActedAt), nullString(r.ActedResult), nullInt(r.ActedOpID),
		nullInt(int64(r.ImportCommandID)), nullTime(r.ImportStartedAt), nullInt(int64(r.ImportFiles)),
		nullInt(int64(r.ImportTotal)))
	return err
}
