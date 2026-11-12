package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/outbox"
)

func t0() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) }

type fakeSender struct {
	mu     sync.Mutex
	fails  int
	sent   []string
	onSend func()
}

func (f *fakeSender) record(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("discord down")
	}
	f.sent = append(f.sent, s)
	if f.onSend != nil {
		f.onSend()
	}
	return nil
}

func (f *fakeSender) NotifyOwner(_ context.Context, c card.Card) error {
	return f.record("owner:" + c.Title)
}

func (f *fakeSender) NotifyUser(_ context.Context, userID string, c card.Card) error {
	return f.record(userID + ":" + c.Title)
}

func (f *fakeSender) SeerrComment(_ context.Context, issueID, text string) error {
	return f.record("comment " + issueID + ":" + text)
}

func (f *fakeSender) SeerrResolve(_ context.Context, issueID string) error {
	return f.record("resolve " + issueID)
}

func (f *fakeSender) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func ownerCard(t *testing.T, d *db.DB, key, title string, at time.Time) {
	t.Helper()
	body, err := json.Marshal(card.Card{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	err = d.EnqueueOutbox(context.Background(),
		db.OutboxRow{Key: key, Target: "owner", Kind: "card", Body: string(body), CreatedAt: at, NextAt: at})
	if err != nil {
		t.Fatal(err)
	}
}

func newAt(d *db.DB, s outbox.Sender, now *time.Time) *outbox.Dispatcher {
	disp := outbox.New(d, s, slog.New(slog.DiscardHandler))
	disp.SetNowForTest(func() time.Time { return *now })
	return disp
}

func TestF9_OutboxDrainsAfterRestart(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ownerCard(t, d, "k1", "escalated", t0())
	now := t0()
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeSender{onSend: cancel}
	newAt(d, s, &now).DrainForTest(ctx)
	newAt(d, s, &now).DrainForTest(context.Background())
	if got := s.got(); !slices.Equal(got, []string{"owner:escalated"}) {
		t.Fatalf("sent %v, want the row once", got)
	}
}

func TestF11_OutboxSendsInCommitOrder(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ownerCard(t, d, "k1", "first", t0())
	ownerCard(t, d, "k2", "second", t0())
	now := t0()
	s := &fakeSender{fails: 1}
	disp := newAt(d, s, &now)
	disp.DrainForTest(context.Background())
	if got := s.got(); len(got) != 0 {
		t.Fatalf("after a failed first send: %v, want nothing (the second waits)", got)
	}
	now = t0().Add(30 * time.Second)
	disp.DrainForTest(context.Background())
	if got := s.got(); len(got) != 0 {
		t.Fatalf("before the first row's retry is due: %v, want nothing", got)
	}
	now = t0().Add(time.Minute)
	disp.DrainForTest(context.Background())
	if got := s.got(); !slices.Equal(got, []string{"owner:first", "owner:second"}) {
		t.Fatalf("order %v, want first then second", got)
	}
}

func TestF9_OutboxRetriesWithBackoff(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ownerCard(t, d, "k1", "x", t0())
	now := t0()
	s := &fakeSender{fails: 100}
	disp := newAt(d, s, &now)
	var waits []time.Duration
	for range 8 {
		disp.DrainForTest(context.Background())
		rows, err := d.DueOutbox(context.Background(), t0().Add(48*time.Hour), 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows %v %v", rows, err)
		}
		waits = append(waits, rows[0].NextAt.Sub(now))
		now = rows[0].NextAt
	}
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	if !slices.Equal(waits[:6], want) {
		t.Fatalf("backoff %v, want %v", waits[:6], want)
	}
	if !now.Equal(t0().Add(24 * time.Hour)) {
		t.Fatalf("last retry at %v, want capped at the 24 h give-up", now)
	}
}

func TestF9_OutboxGivesUpAfter24h(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ownerCard(t, d, "old", "old", t0())
	ownerCard(t, d, "new", "new", t0().Add(23*time.Hour))
	now := t0().Add(24 * time.Hour)
	s := &fakeSender{}
	disp := newAt(d, s, &now)
	disp.DrainForTest(context.Background())
	if got := s.got(); !slices.Equal(got, []string{"owner:new"}) {
		t.Fatalf("sent %v, want only the new row", got)
	}
	if n, err := d.StuckOutboxCount(context.Background(), now); err != nil || n != 1 {
		t.Fatalf("StuckOutboxCount %d %v, want 1", n, err)
	}
}

func TestF9_OutboxRoutesEveryKind(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ctx := context.Background()
	body, _ := json.Marshal(card.Card{Title: "note"})
	for _, r := range []db.OutboxRow{
		{Key: "u", Target: "discord:42", Kind: "card", Body: string(body)},
		{Key: "c", Target: "seerr:7", Kind: "seerr_comment", Body: "looking"},
		{Key: "r", Target: "seerr:7", Kind: "seerr_resolve"},
		{Key: "bad", Target: "pager:1", Kind: "card", Body: string(body)},
	} {
		r.CreatedAt, r.NextAt = t0(), t0()
		if err := d.EnqueueOutbox(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	now := t0()
	s := &fakeSender{}
	newAt(d, s, &now).DrainForTest(ctx)
	if got := s.got(); !slices.Equal(got, []string{"42:note", "comment 7:looking", "resolve 7"}) {
		t.Fatalf("sent %v", got)
	}
	if n, err := d.StuckOutboxCount(ctx, now); err != nil || n != 1 {
		t.Fatalf("an unknown target is given up at once: %d %v", n, err)
	}
}

type untrackedSender struct{ fakeSender }

func (u *untrackedSender) NotifyOwner(_ context.Context, c card.Card) error {
	_ = u.record("owner:" + c.Title)
	return card.ErrUntracked
}

func TestF9_UntrackedCardCountsAsSent(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ownerCard(t, d, "k1", "plain", t0())
	s := &untrackedSender{}
	disp := outbox.New(d, s, slog.New(slog.DiscardHandler))
	disp.Flush(context.Background(), t0())
	disp.Flush(context.Background(), t0().Add(time.Hour))
	if got := s.got(); !slices.Equal(got, []string{"owner:plain"}) {
		t.Fatalf("sent %v, want the row once (an untracked card was delivered)", got)
	}
}

func TestF17_FailureHookQueuesAndIsDrained(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ctx := context.Background()
	body, _ := json.Marshal(card.Card{Title: "note"})
	if err := d.EnqueueOutbox(ctx, db.OutboxRow{Key: "u", Target: "discord:42", Kind: "card", Body: string(body),
		CreatedAt: t0(), NextAt: t0()}); err != nil {
		t.Fatal(err)
	}
	s := &fakeSender{fails: 1}
	disp := outbox.New(d, s, slog.New(slog.DiscardHandler))
	var failed []string
	disp.OnFailed(func(_ context.Context, r db.OutboxRow, _ time.Time, err error) bool {
		failed = append(failed, r.Target+": "+err.Error())
		ownerCard(t, d, "fallback", "could not DM 42", t0())
		return true
	})
	disp.Flush(ctx, t0())
	if !slices.Equal(failed, []string{"discord:42: discord down"}) {
		t.Fatalf("hook saw %v", failed)
	}
	if got := s.got(); !slices.Equal(got, []string{"owner:could not DM 42"}) {
		t.Fatalf("sent %v, want the hook's row in the same flush", got)
	}
}
