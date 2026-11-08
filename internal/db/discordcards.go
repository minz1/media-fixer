package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"time"
)

const discordCardsSchema = `
CREATE TABLE IF NOT EXISTS discord_cards (
	id         INTEGER PRIMARY KEY,
	subject    TEXT NOT NULL,
	channel_id TEXT NOT NULL,
	message_id TEXT NOT NULL,
	card       TEXT NOT NULL,
	hash       TEXT NOT NULL DEFAULT '',
	sent_at    DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS discord_cards_subject ON discord_cards(subject);
CREATE INDEX IF NOT EXISTS discord_cards_message ON discord_cards(message_id);
`

func discordCardsMigrations() []migration {
	return []migration{{
		version: migDiscordCards,
		name:    "discord_cards",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, discordCardsSchema)
			return err
		},
	}}
}

type DiscordCard struct {
	ID         int64
	Subject    string
	ChannelID  string
	MessageID  string
	Card       []byte
	Hash       string
	SentAt     time.Time
	State      string
	LastUpdate json.RawMessage
	LastKnown  bool
}

const (
	CardTracked = "tracked"
	CardGone    = "gone"

	migDiscordCardsRenderState = 23
)

func discordCardsRenderStateMigrations() []migration {
	return []migration{{
		version: migDiscordCardsRenderState,
		name:    "discord_cards_render_state",
		exec: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
ALTER TABLE discord_cards ADD COLUMN state TEXT NOT NULL DEFAULT 'tracked' CHECK (state IN ('tracked', 'gone'));
ALTER TABLE discord_cards ADD COLUMN last_update TEXT CHECK (last_update IS NULL OR json_valid(last_update));
`)
			return err
		},
	}}
}

func (d *DB) InsertDiscordCards(ctx context.Context, rows []DiscordCard) error {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range rows {
		if _, err = tx.ExecContext(
			ctx,
			`INSERT INTO discord_cards (subject, channel_id, message_id, card, hash, sent_at, last_update) VALUES (?, ?, ?, ?, ?, ?, 'null')`,
			r.Subject,
			r.ChannelID,
			r.MessageID,
			string(r.Card),
			r.Hash,
			r.SentAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const discordCardColumns = `SELECT id, subject, channel_id, message_id, card, hash, sent_at, state, last_update FROM discord_cards`

func (d *DB) ListDiscordCards(ctx context.Context, subjectPrefix string) ([]DiscordCard, error) {
	return d.listDiscordCards(
		ctx,
		discordCardColumns+` WHERE substr(subject, 1, length(?)) = ? AND state = 'tracked' ORDER BY sent_at DESC, id DESC`,
		subjectPrefix,
		subjectPrefix,
	)
}

func (d *DB) ListDiscordSubject(ctx context.Context, subject string) ([]DiscordCard, error) {
	return d.listDiscordCards(ctx, discordCardColumns+` WHERE subject = ? AND state = 'tracked' ORDER BY id`, subject)
}

func (d *DB) ListDiscordMessage(ctx context.Context, messageID string) ([]DiscordCard, error) {
	return d.listDiscordCards(ctx, discordCardColumns+` WHERE message_id = ? ORDER BY id`, messageID)
}

func (d *DB) listDiscordCards(ctx context.Context, query string, args ...any) ([]DiscordCard, error) {
	rows, err := d.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscordCard
	for rows.Next() {
		var (
			r    DiscordCard
			body string
			last sql.NullString
		)
		if err = rows.Scan(
			&r.ID, &r.Subject, &r.ChannelID, &r.MessageID, &body, &r.Hash, &r.SentAt, &r.State, &last,
		); err != nil {
			return nil, err
		}
		r.Card = []byte(body)
		if last.Valid {
			r.LastUpdate, r.LastKnown = json.RawMessage(last.String), true
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SetDiscordMessageRendered(
	ctx context.Context, messageID, hash string, updates map[string]json.RawMessage,
) error {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx,
		`UPDATE discord_cards SET hash = ? WHERE message_id = ?`, hash, messageID); err != nil {
		return err
	}
	for _, subject := range slices.Sorted(maps.Keys(updates)) {
		if _, err = tx.ExecContext(ctx,
			`UPDATE discord_cards SET last_update = ? WHERE message_id = ? AND subject = ?`,
			string(updates[subject]), messageID, subject); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) MarkDiscordSubjectsGone(ctx context.Context, messageID string, subjects []string) error {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, subject := range subjects {
		if _, err = tx.ExecContext(ctx,
			`UPDATE discord_cards SET state = ? WHERE message_id = ? AND subject = ?`,
			CardGone, messageID, subject); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) DeleteDiscordMessage(ctx context.Context, messageID string) error {
	_, err := d.write.ExecContext(ctx, `DELETE FROM discord_cards WHERE message_id = ?`, messageID)
	return err
}

func (d *DB) PruneDiscordCards(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := d.write.QueryContext(ctx,
		`DELETE FROM discord_cards WHERE sent_at < ? RETURNING message_id`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}
