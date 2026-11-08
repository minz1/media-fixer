package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

const BabysitTrigger = "babysit:"

const sweepSchema = `
CREATE TABLE IF NOT EXISTS sweep_runs (id INTEGER PRIMARY KEY, trigger TEXT NOT NULL, dry INTEGER NOT NULL,
  started_at DATETIME NOT NULL, finished_at DATETIME, status TEXT NOT NULL, reason TEXT, counts TEXT);
CREATE TABLE IF NOT EXISTS sweep_items (id INTEGER PRIMARY KEY,
  run_id INTEGER NOT NULL REFERENCES sweep_runs(id) ON DELETE CASCADE,
  target TEXT NOT NULL, series_id INTEGER NOT NULL, series_title TEXT NOT NULL, season INTEGER,
  episodes TEXT, release_title TEXT, indexer TEXT, download_id TEXT, result TEXT NOT NULL,
  lookup_results INTEGER, lookup_ms INTEGER, at DATETIME NOT NULL);
CREATE TABLE IF NOT EXISTS sweep_refused (id INTEGER PRIMARY KEY, title_key TEXT NOT NULL, release_title TEXT NOT NULL,
  indexer TEXT NOT NULL, refused_at DATETIME NOT NULL, reason TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS sweep_items_run ON sweep_items(run_id);
CREATE INDEX IF NOT EXISTS sweep_items_target ON sweep_items(target, at);
CREATE INDEX IF NOT EXISTS sweep_refused_key ON sweep_refused(title_key, refused_at);
`

type SweepRun struct {
	ID         int64
	Trigger    string
	Dry        bool
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     sweepstate.RunStatus
	Reason     string
	Counts     SweepCounts
	Items      []SweepItem
}

type SweepCounts struct {
	Grabs       int `json:"grabs"`
	Lookups     int `json:"lookups"`
	Searched    int `json:"searched"`
	Imported    int `json:"imported"`
	Failed      int `json:"failed"`
	Refused     int `json:"refused"`
	Downloading int `json:"downloading"`
	NoPack      int `json:"no_pack"`
	MaxGrabs    int `json:"max_grabs"`
	MaxLookups  int `json:"max_lookups"`
	MaxSearches int `json:"max_searches"`
}

type SweepItem struct {
	ID            int64
	RunID         int64
	Target        string
	SeriesID      int
	SeriesTitle   string
	Season        *int
	Episodes      string
	ReleaseTitle  string
	Indexer       string
	DownloadID    string
	Result        sweepstate.ItemResult
	LookupResults *int
	LookupMS      *int
	At            time.Time
}

type SweepRefused struct {
	ID           int64
	TitleKey     string
	ReleaseTitle string
	Indexer      string
	RefusedAt    time.Time
	Reason       string
	Code         sweepstate.RefusalCode
}

func (d *DB) CreateSweepRun(ctx context.Context, trigger string, dry bool, at time.Time) (int64, error) {
	res, err := d.write.ExecContext(ctx,
		`INSERT INTO sweep_runs (trigger, dry, started_at, status) VALUES (?, ?, ?, 'running')`,
		trigger, dry, at)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

var (
	ErrSweepRunSettled    = errors.New("sweep run already settled")
	ErrSweepFinishInvalid = errors.New("invalid sweep run finish")
	ErrSweepItemMoved     = errors.New("sweep item already moved")
)

func (d *DB) FinishSweepRun(
	ctx context.Context, id int64, status sweepstate.RunStatus, reason string, c SweepCounts, at time.Time,
) error {
	if m := sweepstate.Meta(status); !m.Stored || !m.Settled {
		return fmt.Errorf("%w: %s is not a stored settled status", ErrSweepFinishInvalid, status)
	}
	snap := sweepstate.RunSnap{Status: status, HasCounts: true, HasFinished: true, Reason: reason}
	if err := sweepstate.CheckRun(snap); err != nil {
		return fmt.Errorf("%w: %w", ErrSweepFinishInvalid, err)
	}
	counts, err := json.Marshal(c)
	if err != nil {
		return err
	}
	res, err := d.write.ExecContext(ctx,
		`UPDATE sweep_runs SET status = ?, reason = ?, counts = ?, finished_at = ? WHERE id = ? AND status = ?`,
		string(status), reason, string(counts), at, id, string(sweepstate.Running))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSweepRunSettled
	}
	return nil
}

func (d *DB) CrashRunningSweeps(ctx context.Context) ([]int64, error) {
	rows, err := d.write.QueryContext(ctx,
		`UPDATE sweep_runs SET status = ? WHERE status = ? RETURNING id`,
		string(sweepstate.Interrupted), string(sweepstate.Running))
	if err != nil {
		return nil, err
	}
	return scanIDs(rows)
}

func (d *DB) RunningSweepIDs(ctx context.Context) ([]int64, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id FROM sweep_runs WHERE status = ? ORDER BY id`, string(sweepstate.Running))
	if err != nil {
		return nil, err
	}
	return scanIDs(rows)
}

func scanIDs(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func statusesJSON(pred func(sweepstate.RunMeta) bool) (string, error) {
	statuses := sweepstate.StatusesWhere(pred)
	if statuses == nil {
		statuses = []sweepstate.RunStatus{}
	}
	b, err := json.Marshal(statuses)
	return string(b), err
}

func (d *DB) ListSweepRuns(ctx context.Context, limit int) ([]SweepRun, error) {
	rows, err := d.read.QueryContext(ctx, sweepRunColumns+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []SweepRun
	byID := map[int64]int{}
	for rows.Next() {
		r, scanErr := scanSweepRun(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		byID[r.ID] = len(runs)
		runs = append(runs, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}
	return runs, d.attachSweepItems(ctx, runs, byID, runs[len(runs)-1].ID)
}

const crashedRunSQL = `(status = 'interrupted' AND counts IS NULL)`

const sweepRunColumns = `SELECT id, trigger, dry, started_at, finished_at, status, COALESCE(reason, ''), counts
	FROM sweep_runs`

func (d *DB) GetSweepRun(ctx context.Context, id int64) (SweepRun, error) {
	r, err := scanSweepRun(d.read.QueryRowContext(ctx, sweepRunColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SweepRun{}, ErrNotFound
	}
	return r, err
}

func scanSweepRun(row interface{ Scan(dest ...any) error }) (SweepRun, error) {
	var (
		r        SweepRun
		finished sql.NullTime
		counts   sql.NullString
		stored   sweepstate.RunStatus
	)
	if err := row.Scan(&r.ID, &r.Trigger, &r.Dry, &r.StartedAt, &finished, &stored, &r.Reason, &counts); err != nil {
		return SweepRun{}, err
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	if counts.Valid {
		if err := json.Unmarshal([]byte(counts.String), &r.Counts); err != nil {
			return SweepRun{}, err
		}
	}
	r.Status = sweepstate.Derive(stored, counts.Valid)
	return r, nil
}

const sweepItemColumns = `i.id, i.run_id, i.target, i.series_id, i.series_title, i.season, COALESCE(i.episodes, ''),
	COALESCE(i.release_title, ''), COALESCE(i.indexer, ''), COALESCE(i.download_id, ''), i.result,
	i.lookup_results, i.lookup_ms, i.at`

func scanSweepItem(row interface{ Scan(dest ...any) error }) (SweepItem, error) {
	var it SweepItem
	err := row.Scan(&it.ID, &it.RunID, &it.Target, &it.SeriesID, &it.SeriesTitle, &it.Season, &it.Episodes,
		&it.ReleaseTitle, &it.Indexer, &it.DownloadID, &it.Result, &it.LookupResults, &it.LookupMS, &it.At)
	return it, err
}

func (d *DB) SeriesSweepItems(ctx context.Context, seriesID, limit int) ([]SweepItem, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+sweepItemColumns+` FROM sweep_items i JOIN sweep_runs r ON r.id = i.run_id
		 WHERE r.dry = 0 AND i.series_id = ? ORDER BY i.at DESC, i.id DESC LIMIT ?`, seriesID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SweepItem
	for rows.Next() {
		it, scanErr := scanSweepItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (d *DB) LatestSweepItems(ctx context.Context) (map[int]SweepItem, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT `+sweepItemColumns+` FROM sweep_items i JOIN (
		   SELECT s.id, ROW_NUMBER() OVER (PARTITION BY s.series_id ORDER BY s.at DESC, s.id DESC) AS rn
		   FROM sweep_items s JOIN sweep_runs r ON r.id = s.run_id WHERE r.dry = 0) w ON w.id = i.id
		 WHERE w.rn = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]SweepItem{}
	for rows.Next() {
		it, scanErr := scanSweepItem(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out[it.SeriesID] = it
	}
	return out, rows.Err()
}

func (d *DB) attachSweepItems(ctx context.Context, runs []SweepRun, byID map[int64]int, minRunID int64) error {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id, run_id, target, series_id, series_title, season, COALESCE(episodes, ''),
		        COALESCE(release_title, ''), COALESCE(indexer, ''), COALESCE(download_id, ''), result,
		        lookup_results, lookup_ms, at
		 FROM sweep_items WHERE run_id >= ? ORDER BY id`, minRunID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		it, scanErr := scanSweepItem(rows)
		if scanErr != nil {
			return scanErr
		}
		if i, ok := byID[it.RunID]; ok {
			runs[i].Items = append(runs[i].Items, it)
		}
	}
	return rows.Err()
}

func (d *DB) LastSweepRunStart(ctx context.Context) (*time.Time, error) {
	exclude, err := statusesJSON(func(m sweepstate.RunMeta) bool { return m.Stored && !m.AnchorsSlot })
	if err != nil {
		return nil, err
	}
	var at time.Time
	err = d.read.QueryRowContext(ctx,
		`SELECT started_at FROM sweep_runs
		 WHERE dry = 0 AND trigger NOT LIKE ? AND status NOT IN (SELECT value FROM json_each(?))
		 ORDER BY id DESC LIMIT 1`, BabysitTrigger+"%", exclude).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &at, nil
}

func (d *DB) PruneSweepRuns(ctx context.Context, keep int, before time.Time) error {
	_, err := d.write.ExecContext(ctx,
		`DELETE FROM sweep_runs WHERE started_at < ?
		 AND id NOT IN (SELECT id FROM sweep_runs ORDER BY id DESC LIMIT ?)`, before, keep)
	return err
}

func (d *DB) AddSweepItem(ctx context.Context, it *SweepItem) error {
	res, err := d.write.ExecContext(ctx,
		`INSERT INTO sweep_items (run_id, target, series_id, series_title, season, episodes, release_title,
		   indexer, download_id, result, lookup_results, lookup_ms, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		it.RunID, it.Target, it.SeriesID, it.SeriesTitle, it.Season, it.Episodes, it.ReleaseTitle,
		it.Indexer, it.DownloadID, it.Result, it.LookupResults, it.LookupMS, it.At)
	if err != nil {
		return err
	}
	it.ID, err = res.LastInsertId()
	return err
}

func (d *DB) UpdateSweepItem(
	ctx context.Context, id int64, from, to sweepstate.ItemResult, downloadID string,
) error {
	if err := sweepstate.Move(from, to); err != nil {
		return err
	}
	res, err := d.write.ExecContext(ctx,
		`UPDATE sweep_items SET result = ?, download_id = COALESCE(NULLIF(?, ''), download_id)
		 WHERE id = ? AND result = ?`,
		string(to), downloadID, id, string(from))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSweepItemMoved
	}
	return nil
}

func (d *DB) AddSweepRefused(ctx context.Context, r *SweepRefused) error {
	res, err := d.write.ExecContext(ctx,
		`INSERT INTO sweep_refused (title_key, release_title, indexer, refused_at, reason, code)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.TitleKey, r.ReleaseTitle, r.Indexer, r.RefusedAt, r.Reason, nullableCode(r.Code))
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

func (d *DB) ListSweepRefused(ctx context.Context, since time.Time) ([]SweepRefused, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id, title_key, release_title, indexer, refused_at, reason, COALESCE(code, `+refusalCodeSQL+`)
		 FROM sweep_refused WHERE refused_at >= ? ORDER BY refused_at DESC, id DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SweepRefused
	for rows.Next() {
		var r SweepRefused
		if err = rows.Scan(
			&r.ID,
			&r.TitleKey,
			&r.ReleaseTitle,
			&r.Indexer,
			&r.RefusedAt,
			&r.Reason,
			&r.Code,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) DeleteSweepRefused(ctx context.Context, id int64) error {
	_, err := d.write.ExecContext(ctx, `DELETE FROM sweep_refused WHERE id = ?`, id)
	return err
}

func (d *DB) RetryableSweepRunsSince(ctx context.Context, since time.Time) (int, time.Time, error) {
	retryable, err := statusesJSON(func(m sweepstate.RunMeta) bool { return m.Stored && m.Retryable })
	if err != nil {
		return 0, time.Time{}, err
	}
	rows, err := d.read.QueryContext(ctx,
		`SELECT started_at, finished_at FROM sweep_runs
		 WHERE dry = 0 AND trigger NOT LIKE ? AND status IN (SELECT value FROM json_each(?)) AND started_at >= ?`,
		BabysitTrigger+"%", retryable, since)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer rows.Close()
	n, latest := 0, time.Time{}
	for rows.Next() {
		var started time.Time
		var finished *time.Time
		if err = rows.Scan(&started, &finished); err != nil {
			return 0, time.Time{}, err
		}
		if finished != nil {
			started = *finished
		}
		n++
		if started.After(latest) {
			latest = started
		}
	}
	return n, latest, rows.Err()
}

func (d *DB) LiveGrabsSince(ctx context.Context, since time.Time) (int, error) {
	grabs := sweepstate.ItemsWhere(func(m sweepstate.ItemMeta) bool { return m.Grab })
	list, err := json.Marshal(grabs)
	if err != nil {
		return 0, err
	}
	var n int
	err = d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sweep_items i JOIN sweep_runs r ON r.id = i.run_id
		 WHERE r.dry = 0 AND i.at >= ? AND COALESCE(i.release_title, '') != ''
		   AND i.result IN (SELECT value FROM json_each(?))`,
		since, string(list)).Scan(&n)
	return n, err
}

func (d *DB) LiveLookupsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sweep_items i JOIN sweep_runs r ON r.id = i.run_id
		 WHERE r.dry = 0 AND i.at >= ? AND i.lookup_results IS NOT NULL`, since).Scan(&n)
	return n, err
}

func (d *DB) LiveSearchesSince(ctx context.Context, since time.Time) (int, bool, error) {
	var searched, crashed int
	err := d.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(json_extract(counts, '$.searched')), 0),
		        COALESCE(SUM(`+crashedRunSQL+`), 0)
		 FROM sweep_runs WHERE dry = 0 AND COALESCE(finished_at,
		   (SELECT MAX(at) FROM sweep_items WHERE run_id = sweep_runs.id), started_at) >= ?`, since).Scan(&searched, &crashed)
	return searched, crashed > 0, err
}

const migSweepRefusalCode = 25

const sweepRefusalAlter = `ALTER TABLE sweep_refused ADD COLUMN code TEXT
  CHECK (code IS NULL OR code IN ('torrent_blocked', 'provider_error', 'other'))`

const sweepRefusalCaseFrozen = `CASE WHEN instr(reason, 'torrent_blocked') > 0 THEN 'torrent_blocked'
       WHEN instr(reason, 'provider_error') > 0 THEN 'provider_error'
       ELSE 'other' END`

const sweepRefusalBackfill = `UPDATE sweep_refused SET code = ` + sweepRefusalCaseFrozen + ` WHERE code IS NULL`

const refusalCodeSQL = `CASE WHEN instr(reason, 'torrent_blocked') > 0 THEN 'torrent_blocked'
       WHEN instr(reason, 'provider_error') > 0 THEN 'provider_error'
       ELSE 'other' END`

func sweepMigrations() []migration {
	return []migration{{
		version: migSweepRefusalCode,
		name:    "sweep_refused_code",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, sweepRefusalAlter); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, sweepRefusalBackfill)
			return err
		},
	}}
}

func (d *DB) PruneSweepRefused(ctx context.Context, now time.Time) error {
	return runTx(ctx, d.write, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sweep_refused SET code = `+refusalCodeSQL+` WHERE code IS NULL`); err != nil {
			return err
		}
		for _, code := range sweepstate.RefusalCodes() {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sweep_refused WHERE code = ? AND refused_at < ?`,
				string(code), now.Add(-code.Memory())); err != nil {
				return err
			}
		}
		return nil
	})
}

func nullableCode(c sweepstate.RefusalCode) any {
	if c == "" {
		return nil
	}
	return string(c)
}
