package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const sweepTargetsSchema = `
CREATE TABLE IF NOT EXISTS sweep_targets (
	target        TEXT PRIMARY KEY,
	series_id     INTEGER NOT NULL,
	series_title  TEXT NOT NULL DEFAULT '',
	season        INTEGER NOT NULL,
	streak        INTEGER NOT NULL DEFAULT 0,
	missing       INTEGER NOT NULL,
	attempts      INTEGER NOT NULL DEFAULT 0,
	last_attempt  DATETIME NOT NULL,
	next_eligible DATETIME NOT NULL
);
`

func sweepTargetsMigrations() []migration {
	return []migration{{
		version: migSweepTargets,
		name:    "sweep_targets",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, sweepTargetsSchema)
			return err
		},
	}}
}

type SweepTarget struct {
	Target                            string
	SeriesID                          int
	SeriesTitle                       string
	Season, Streak, Missing, Attempts int
	LastAttempt, NextEligible         time.Time
}

func (d *DB) ListSweepTargets(ctx context.Context) (map[string]SweepTarget, error) {
	rows, err := d.read.QueryContext(ctx, `SELECT target, series_id, series_title, season, streak, missing,
		attempts, last_attempt, next_eligible FROM sweep_targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SweepTarget{}
	for rows.Next() {
		var t SweepTarget
		if err = rows.Scan(&t.Target, &t.SeriesID, &t.SeriesTitle, &t.Season, &t.Streak, &t.Missing,
			&t.Attempts, &t.LastAttempt, &t.NextEligible); err != nil {
			return nil, err
		}
		out[t.Target] = t
	}
	return out, rows.Err()
}

func (d *DB) UpsertSweepTarget(ctx context.Context, t SweepTarget) error {
	_, err := d.write.ExecContext(ctx, `INSERT INTO sweep_targets (target, series_id, series_title, season, streak,
		missing, attempts, last_attempt, next_eligible) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(target) DO UPDATE SET series_id = excluded.series_id, series_title = excluded.series_title,
		season = excluded.season, streak = excluded.streak, missing = excluded.missing,
		attempts = excluded.attempts, last_attempt = excluded.last_attempt, next_eligible = excluded.next_eligible`,
		t.Target, t.SeriesID, t.SeriesTitle, t.Season, t.Streak, t.Missing, t.Attempts, t.LastAttempt, t.NextEligible)
	return err
}

func (d *DB) DeleteSweepTargets(ctx context.Context, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	keys, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	_, err = d.write.ExecContext(ctx,
		`DELETE FROM sweep_targets WHERE target IN (SELECT value FROM json_each(?))`, string(keys))
	return err
}
