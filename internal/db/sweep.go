package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

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
	Status     string
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
	Result        string
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

func (d *DB) FinishSweepRun(ctx context.Context, id int64, status, reason string, c SweepCounts, at time.Time) error {
	counts, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = d.write.ExecContext(ctx,
		`UPDATE sweep_runs SET status = ?, reason = ?, counts = ?, finished_at = ? WHERE id = ?`,
		status, reason, string(counts), at, id)
	return err
}

func (d *DB) InterruptRunningSweeps(ctx context.Context) (int, error) {
	res, err := d.write.ExecContext(ctx, `UPDATE sweep_runs SET status = 'interrupted' WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (d *DB) ListSweepRuns(ctx context.Context, limit int) ([]SweepRun, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id, trigger, dry, started_at, finished_at, status, COALESCE(reason, ''), counts
		 FROM sweep_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []SweepRun
	byID := map[int64]int{}
	for rows.Next() {
		var (
			r        SweepRun
			finished sql.NullTime
			counts   sql.NullString
		)
		if err = rows.Scan(
			&r.ID,
			&r.Trigger,
			&r.Dry,
			&r.StartedAt,
			&finished,
			&r.Status,
			&r.Reason,
			&counts,
		); err != nil {
			return nil, err
		}
		if finished.Valid {
			r.FinishedAt = &finished.Time
		}
		if counts.Valid {
			if err = json.Unmarshal([]byte(counts.String), &r.Counts); err != nil {
				return nil, err
			}
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
		var it SweepItem
		if err = rows.Scan(
			&it.ID,
			&it.RunID,
			&it.Target,
			&it.SeriesID,
			&it.SeriesTitle,
			&it.Season,
			&it.Episodes,
			&it.ReleaseTitle,
			&it.Indexer,
			&it.DownloadID,
			&it.Result,
			&it.LookupResults,
			&it.LookupMS,
			&it.At,
		); err != nil {
			return err
		}
		if i, ok := byID[it.RunID]; ok {
			runs[i].Items = append(runs[i].Items, it)
		}
	}
	return rows.Err()
}

func (d *DB) LastSweepRunStart(ctx context.Context, excludeStatus []string) (*time.Time, error) {
	exclude, err := json.Marshal(append([]string{}, excludeStatus...))
	if err != nil {
		return nil, err
	}
	var at time.Time
	err = d.read.QueryRowContext(ctx,
		`SELECT started_at FROM sweep_runs WHERE dry = 0 AND status NOT IN (SELECT value FROM json_each(?))
		 ORDER BY id DESC LIMIT 1`, string(exclude)).Scan(&at)
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

func (d *DB) UpdateSweepItem(ctx context.Context, id int64, result, downloadID string) error {
	_, err := d.write.ExecContext(ctx,
		`UPDATE sweep_items SET result = ?, download_id = COALESCE(NULLIF(?, ''), download_id) WHERE id = ?`,
		result, downloadID, id)
	return err
}

func (d *DB) NoPackTargets(ctx context.Context, since time.Time) (map[string]bool, error) {
	return d.stringSet(ctx,
		`SELECT DISTINCT i.target FROM sweep_items i JOIN sweep_runs r ON r.id = i.run_id
		 WHERE r.dry = 0 AND i.result = 'no_pack' AND i.at >= ?`, since)
}

func (d *DB) AddSweepRefused(ctx context.Context, r *SweepRefused) error {
	res, err := d.write.ExecContext(ctx,
		`INSERT INTO sweep_refused (title_key, release_title, indexer, refused_at, reason) VALUES (?, ?, ?, ?, ?)`,
		r.TitleKey, r.ReleaseTitle, r.Indexer, r.RefusedAt, r.Reason)
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

func (d *DB) ListSweepRefused(ctx context.Context, since time.Time) ([]SweepRefused, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT id, title_key, release_title, indexer, refused_at, reason FROM sweep_refused
		 WHERE refused_at >= ? ORDER BY refused_at DESC, id DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SweepRefused
	for rows.Next() {
		var r SweepRefused
		if err = rows.Scan(&r.ID, &r.TitleKey, &r.ReleaseTitle, &r.Indexer, &r.RefusedAt, &r.Reason); err != nil {
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

func (d *DB) stringSet(ctx context.Context, query string, args ...any) (map[string]bool, error) {
	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	var s string
	for rows.Next() {
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}

func (d *DB) RetryableSweepRunsSince(ctx context.Context, since time.Time) (int, time.Time, error) {
	rows, err := d.read.QueryContext(ctx,
		`SELECT started_at, finished_at FROM sweep_runs
		 WHERE dry = 0 AND status IN ('failed', 'interrupted') AND started_at >= ?`, since)
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
	var n int
	err := d.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sweep_items i JOIN sweep_runs r ON r.id = i.run_id
		 WHERE r.dry = 0 AND r.started_at >= ?
		   AND i.result IN ('grabbing', 'imported', 'failed', 'refused', 'archive', 'downloading')`,
		since).Scan(&n)
	return n, err
}
