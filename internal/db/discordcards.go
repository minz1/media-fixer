package db

import (
	"context"
	"database/sql"
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
	ID        int64
	Subject   string
	ChannelID string
	MessageID string
	Card      []byte
	Hash      string
	SentAt    time.Time
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
			`INSERT INTO discord_cards (subject, channel_id, message_id, card, hash, sent_at) VALUES (?, ?, ?, ?, ?, ?)`,
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

const discordCardColumns = `SELECT id, subject, channel_id, message_id, card, hash, sent_at FROM discord_cards`

func (d *DB) ListDiscordCards(ctx context.Context, subjectPrefix string) ([]DiscordCard, error) {
	return d.listDiscordCards(ctx,
		discordCardColumns+` WHERE substr(subject, 1, length(?)) = ? ORDER BY sent_at DESC, id DESC`,
		subjectPrefix, subjectPrefix)
}

func (d *DB) ListDiscordSubject(ctx context.Context, subject string) ([]DiscordCard, error) {
	return d.listDiscordCards(ctx, discordCardColumns+` WHERE subject = ? ORDER BY id`, subject)
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
		)
		if err = rows.Scan(&r.ID, &r.Subject, &r.ChannelID, &r.MessageID, &body, &r.Hash, &r.SentAt); err != nil {
			return nil, err
		}
		r.Card = []byte(body)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SetDiscordMessageHash(ctx context.Context, messageID, hash string) error {
	_, err := d.write.ExecContext(ctx, `UPDATE discord_cards SET hash = ? WHERE message_id = ?`, hash, messageID)
	return err
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
