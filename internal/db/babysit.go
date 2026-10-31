package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const babysitSchema = `
CREATE TABLE IF NOT EXISTS babysit_explanations (
	series_id   INTEGER PRIMARY KEY,
	fingerprint TEXT NOT NULL,
	text        TEXT NOT NULL,
	model       TEXT NOT NULL,
	created_at  DATETIME NOT NULL
);
`

func babysitMigrations() []migration {
	return []migration{{
		version: migBabysitExplanations,
		name:    "babysit_explanations",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, babysitSchema)
			return err
		},
	}}
}

type BabysitExplanation struct {
	SeriesID    int
	Fingerprint string
	Text        string
	Model       string
	CreatedAt   time.Time
}

func (d *DB) GetBabysitExplanation(ctx context.Context, seriesID int) (BabysitExplanation, error) {
	e := BabysitExplanation{SeriesID: seriesID}
	err := d.read.QueryRowContext(ctx,
		`SELECT fingerprint, text, model, created_at FROM babysit_explanations WHERE series_id = ?`, seriesID).
		Scan(&e.Fingerprint, &e.Text, &e.Model, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BabysitExplanation{}, ErrNotFound
	}
	e.CreatedAt = e.CreatedAt.UTC()
	return e, err
}

func (d *DB) PutBabysitExplanation(ctx context.Context, e BabysitExplanation) error {
	_, err := d.write.ExecContext(ctx,
		`INSERT OR REPLACE INTO babysit_explanations (series_id, fingerprint, text, model, created_at)
		 VALUES (?, ?, ?, ?, ?)`, e.SeriesID, e.Fingerprint, e.Text, e.Model, e.CreatedAt.UTC())
	return err
}
