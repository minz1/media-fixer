package db_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
)

const v20InsertDiscordCard = `INSERT INTO discord_cards (subject, channel_id, message_id, card, hash, sent_at) VALUES (?, ?, ?, ?, ?, ?)`

func TestMigration23_AddsRenderStateColumns(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m23.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw := rawOpen(t, path)
	defer raw.Close()
	if v := countInt(t, raw, `SELECT MAX(version) FROM schema_version`); v != 23 {
		t.Fatalf("schema version %d, want 23", v)
	}
	cols := map[string]string{}
	rows, err := raw.QueryContext(context.Background(),
		`SELECT name, COALESCE(dflt_value, '') FROM pragma_table_info('discord_cards')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, dflt string
		if err = rows.Scan(&name, &dflt); err != nil {
			t.Fatal(err)
		}
		cols[name] = dflt
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if dflt, ok := cols["state"]; !ok || dflt != "'tracked'" {
		t.Errorf("state column default %q (present %v), want 'tracked'", dflt, ok)
	}
	if dflt, ok := cols["last_update"]; !ok || dflt != "" {
		t.Errorf("last_update column default %q (present %v), want none", dflt, ok)
	}
}

func TestMigration23_ChecksRejectBadValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m23.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw := rawOpen(t, path)
	defer raw.Close()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO discord_cards (subject, channel_id, message_id, card, sent_at, state)
		 VALUES ('inc:a', 'C', 'M', '{}', CURRENT_TIMESTAMP, 'x')`,
		`INSERT INTO discord_cards (subject, channel_id, message_id, card, sent_at, last_update)
		 VALUES ('inc:a', 'C', 'M', '{}', CURRENT_TIMESTAMP, '{')`,
	} {
		if _, err = raw.ExecContext(ctx, q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
}

func TestMigration23_OldInsertStillWorks(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m23.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw := rawOpen(t, path)
	if _, err = raw.ExecContext(context.Background(), v20InsertDiscordCard,
		"inc:a", "C", "M", `{"title":"T"}`, "h0", cardT0()); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if d, err = db.Open(path); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.ListDiscordMessage(context.Background(), "M")
	if err != nil || len(got) != 1 {
		t.Fatalf("rows %v err %v", got, err)
	}
	if got[0].State != db.CardTracked || got[0].LastKnown || got[0].LastUpdate != nil {
		t.Errorf("row = %+v, want tracked with an unknown last update", got[0])
	}
}

func TestMigration23_OnSnapshot(t *testing.T) {
	t.Parallel()
	src := os.Getenv("MF_SNAPSHOT")
	if src == "" {
		t.Skip("MF_SNAPSHOT not set")
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	copyFile(t, src, path)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	all, err := d.ListDiscordCards(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	messages := map[string]bool{}
	for _, r := range all {
		messages[r.MessageID] = true
	}
	for id := range messages {
		rows, lerr := d.ListDiscordMessage(ctx, id)
		if lerr != nil || len(rows) == 0 {
			t.Fatalf("message %s: rows %d err %v", id, len(rows), lerr)
		}
		for _, r := range rows {
			if r.State != db.CardTracked || r.LastKnown {
				t.Errorf("row %d: state %q last known %v", r.ID, r.State, r.LastKnown)
			}
		}
	}
	d.Close()
	raw := rawOpen(t, path)
	defer raw.Close()
	t.Logf("discord_cards rows %d, messages %d", len(all), len(messages))
	if len(all) != 16 || len(messages) != 16 {
		t.Errorf("rows %d messages %d, want 16 and 16", len(all), len(messages))
	}
	if n := countInt(
		t,
		raw,
		`SELECT COUNT(*) FROM discord_cards WHERE state = 'tracked' AND last_update IS NULL`,
	); n != 16 {
		t.Errorf("tracked rows with SQL NULL last_update = %d, want 16", n)
	}
	var check string
	if err = raw.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Errorf("integrity_check %q err %v", check, err)
	}
}

func TestInsertDiscordCards_WritesJSONNullLast(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	insertCards(t, d, "M1", cardT0(), "inc:a")
	got, err := d.ListDiscordMessage(context.Background(), "M1")
	if err != nil || len(got) != 1 {
		t.Fatalf("rows %v err %v", got, err)
	}
	if !got[0].LastKnown || string(got[0].LastUpdate) != "null" || got[0].State != db.CardTracked {
		t.Errorf("row = %+v, want tracked with JSON null", got[0])
	}
}

func TestSetDiscordMessageRendered_HashAndUpdatesInOneTx(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertCards(t, d, "M1", cardT0(), "inc:a", "sq:s:b")
	if err := d.SetDiscordMessageRendered(ctx, "M1", "h1", map[string]json.RawMessage{
		"inc:a": json.RawMessage(`{"status":"x"}`),
	}); err != nil {
		t.Fatal(err)
	}
	got := byMessageSubject(t, d, "M1")
	if got["inc:a"].Hash != "h1" || got["sq:s:b"].Hash != "h1" || string(got["inc:a"].LastUpdate) != `{"status":"x"}` ||
		string(got["sq:s:b"].LastUpdate) != "null" {
		t.Fatalf("rows after store = %+v", got)
	}
	if err := d.SetDiscordMessageRendered(ctx, "M1", "h2", map[string]json.RawMessage{
		"inc:a":  json.RawMessage(`{"status":"y"}`),
		"sq:s:b": json.RawMessage(`{`),
	}); err == nil {
		t.Fatal("an invalid update was stored")
	}
	got = byMessageSubject(t, d, "M1")
	if got["inc:a"].Hash != "h1" || string(got["inc:a"].LastUpdate) != `{"status":"x"}` {
		t.Errorf("a failed store changed the rows: %+v", got)
	}
}

func TestMarkDiscordSubjectsGone(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertCards(t, d, "M1", cardT0(), "inc:a", "inc:b")
	insertCards(t, d, "M2", cardT0(), "inc:a")
	if err := d.MarkDiscordSubjectsGone(ctx, "M1", []string{"inc:a"}); err != nil {
		t.Fatal(err)
	}
	got := byMessageSubject(t, d, "M1")
	if got["inc:a"].State != db.CardGone || got["inc:b"].State != db.CardTracked {
		t.Errorf("M1 rows = %+v", got)
	}
	if other := byMessageSubject(t, d, "M2"); other["inc:a"].State != db.CardTracked {
		t.Errorf("M2 row = %+v, want tracked", other["inc:a"])
	}
}

func TestListDiscordSubject_SkipsGone(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertCards(t, d, "M1", cardT0(), "inc:a")
	insertCards(t, d, "M2", cardT0(), "inc:a")
	if err := d.MarkDiscordSubjectsGone(ctx, "M1", []string{"inc:a"}); err != nil {
		t.Fatal(err)
	}
	got, err := d.ListDiscordSubject(ctx, "inc:a")
	if err != nil || len(got) != 1 || got[0].MessageID != "M2" {
		t.Errorf("rows %+v err %v, want only M2", got, err)
	}
}

func TestListDiscordCardsPrefix_SkipsGone(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	insertCards(t, d, "M1", cardT0(), "sweep:1", "sweep:2")
	if err := d.MarkDiscordSubjectsGone(ctx, "M1", []string{"sweep:1"}); err != nil {
		t.Fatal(err)
	}
	got, err := d.ListDiscordCards(ctx, "sweep:")
	if err != nil || len(got) != 1 || got[0].Subject != "sweep:2" {
		t.Errorf("rows %+v err %v, want only sweep:2", got, err)
	}
}

func TestListDiscordMessage_ReturnsStateAndLastUpdate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "m23.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	insertCards(t, d, "M1", cardT0(), "inc:a", "inc:b")
	if err = d.MarkDiscordSubjectsGone(context.Background(), "M1", []string{"inc:b"}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	raw := rawOpen(t, path)
	if _, err = raw.ExecContext(context.Background(), v20InsertDiscordCard,
		"inc:c", "C", "M1", `{"title":"T"}`, "h0", cardT0()); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if d, err = db.Open(path); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got := byMessageSubject(t, d, "M1")
	if len(got) != 3 {
		t.Fatalf("rows = %+v, want 3 including the gone one", got)
	}
	if a := got["inc:a"]; a.State != db.CardTracked || !a.LastKnown || string(a.LastUpdate) != "null" {
		t.Errorf("inc:a = %+v, want tracked with JSON null", a)
	}
	if b := got["inc:b"]; b.State != db.CardGone || !b.LastKnown {
		t.Errorf("inc:b = %+v, want gone", b)
	}
	if c := got["inc:c"]; c.State != db.CardTracked || c.LastKnown || c.LastUpdate != nil {
		t.Errorf("inc:c = %+v, want tracked with SQL NULL", c)
	}
}

func byMessageSubject(t *testing.T, d *db.DB, messageID string) map[string]db.DiscordCard {
	t.Helper()
	rows, err := d.ListDiscordMessage(context.Background(), messageID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]db.DiscordCard, len(rows))
	for _, r := range rows {
		out[r.Subject] = r
	}
	return out
}
