package db_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func cardT0() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }

func insertCards(t *testing.T, d *db.DB, messageID string, at time.Time, subjects ...string) {
	t.Helper()
	rows := make([]db.DiscordCard, 0, len(subjects))
	for _, s := range subjects {
		rows = append(rows, db.DiscordCard{
			Subject: s, ChannelID: "C", MessageID: messageID, Card: []byte(`{"title":"T"}`), Hash: "h0", SentAt: at,
		})
	}
	if err := d.InsertDiscordCards(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
}

func subjects(rows []db.DiscordCard) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Subject)
	}
	return out
}

func TestDiscordCards_ListByPrefix(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	insertCards(t, d, "M1", cardT0(), "sq:s:a", "sq:r:b")
	insertCards(t, d, "M2", cardT0().Add(time.Minute), "inc:x")
	got, err := d.ListDiscordCards(context.Background(), "sq:")
	if err != nil {
		t.Fatal(err)
	}
	if s := subjects(got); len(s) != 2 || !slices.Contains(s, "sq:s:a") || !slices.Contains(s, "sq:r:b") {
		t.Fatalf("sq: rows = %v", s)
	}
	all, err := d.ListDiscordCards(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Subject != "inc:x" {
		t.Fatalf("all rows = %v, want 3 newest first", subjects(all))
	}
	r := all[0]
	if r.ID == 0 || r.ChannelID != "C" || r.MessageID != "M2" || string(r.Card) != `{"title":"T"}` ||
		r.Hash != "h0" || !r.SentAt.Equal(cardT0().Add(time.Minute)) {
		t.Fatalf("row = %+v", r)
	}
}

func TestDiscordCards_ExactSubject(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	insertCards(t, d, "M1", cardT0(), "sq:s:aXb")
	got, err := d.ListDiscordSubject(context.Background(), "sq:s:a_b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("sq:s:a_b matched %v", subjects(got))
	}
	insertCards(t, d, "M2", cardT0(), "sq:s:a_b")
	if got, err = d.ListDiscordSubject(context.Background(), "sq:s:a_b"); err != nil || len(got) != 1 {
		t.Fatalf("rows = %v, err = %v", subjects(got), err)
	}
}

func TestDiscordCards_MessageHashAndDelete(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertCards(t, d, "M1", cardT0(), "sq:s:a", "sq:s:b")
	insertCards(t, d, "M2", cardT0(), "inc:x")
	if err := d.SetDiscordMessageHash(ctx, "M1", "h1"); err != nil {
		t.Fatal(err)
	}
	m1, err := d.ListDiscordMessage(ctx, "M1")
	if err != nil {
		t.Fatal(err)
	}
	if len(m1) != 2 || m1[0].Hash != "h1" || m1[1].Hash != "h1" {
		t.Fatalf("M1 rows = %+v", m1)
	}
	if m2, _ := d.ListDiscordMessage(ctx, "M2"); len(m2) != 1 || m2[0].Hash != "h0" {
		t.Fatalf("M2 rows = %+v", m2)
	}
	if err = d.DeleteDiscordMessage(ctx, "M1"); err != nil {
		t.Fatal(err)
	}
	if m1, _ = d.ListDiscordMessage(ctx, "M1"); len(m1) != 0 {
		t.Fatalf("M1 rows after delete = %+v", m1)
	}
	if m2, _ := d.ListDiscordMessage(ctx, "M2"); len(m2) != 1 {
		t.Fatalf("M2 rows after deleting M1 = %+v", m2)
	}
}

func TestDiscordCards_Prune(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertCards(t, d, "OLD", cardT0().Add(-15*24*time.Hour), "sq:s:a", "sq:s:b")
	insertCards(t, d, "NEW", cardT0(), "inc:x")
	pruned, err := d.PruneDiscordCards(ctx, cardT0().Add(-14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != "OLD" {
		t.Fatalf("pruned = %v, want [OLD]", pruned)
	}
	all, err := d.ListDiscordCards(ctx, "")
	if err != nil || len(all) != 1 || all[0].MessageID != "NEW" {
		t.Fatalf("left = %+v, err = %v", all, err)
	}
}

func TestDiscordCardsMigration_CreatesTableOnExistingDB(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	rawExec(t, path, `DROP TABLE discord_cards`, `DELETE FROM schema_version WHERE version = 18`)

	d2, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if _, err = d2.ListDiscordCards(context.Background(), ""); err != nil {
		t.Errorf("table not recreated on a v17 db: %v", err)
	}
	got := rawQuery(t, path, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index'
		AND name IN ('discord_cards_subject', 'discord_cards_message')`)
	if got != "2" {
		t.Errorf("discord_cards indexes = %s, want 2", got)
	}
}
