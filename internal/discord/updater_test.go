package discord_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/discord"
)

const waitLimit = 3 * time.Second

type editSender struct {
	fakeSender

	emu     sync.Mutex
	sent    int
	edits   []*discordgo.MessageEdit
	editErr error
	block   chan struct{}

	hold     func(ctx context.Context, id string) error
	errQueue []error
	errFor   map[string]error
	retried  []bool
}

func (f *editSender) ChannelMessageSendComplex(
	ch string, m *discordgo.MessageSend, o ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	if _, err := f.fakeSender.ChannelMessageSendComplex(ch, m, o...); err != nil {
		return nil, err
	}
	f.emu.Lock()
	defer f.emu.Unlock()
	f.sent++
	return &discordgo.Message{ID: fmt.Sprintf("sent%d", f.sent), ChannelID: ch}, nil
}

func (f *editSender) ChannelMessageEditComplex(
	m *discordgo.MessageEdit, opts ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	cfg := discordgo.RequestConfig{
		Request:                httptest.NewRequest(http.MethodPatch, "/", nil),
		ShouldRetryOnRateLimit: true,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if f.hold != nil {
		if err := f.hold(cfg.Request.Context(), m.ID); err != nil {
			return nil, err
		}
	}
	if f.block != nil {
		<-f.block
	}
	f.emu.Lock()
	defer f.emu.Unlock()
	f.edits = append(f.edits, m)
	f.retried = append(f.retried, cfg.ShouldRetryOnRateLimit)
	if len(f.errQueue) > 0 {
		err := f.errQueue[0]
		f.errQueue = f.errQueue[1:]
		return &discordgo.Message{}, err
	}
	if err, ok := f.errFor[m.ID]; ok {
		return &discordgo.Message{}, err
	}
	return &discordgo.Message{}, f.editErr
}

func (f *editSender) editList() []*discordgo.MessageEdit {
	f.emu.Lock()
	defer f.emu.Unlock()
	return append([]*discordgo.MessageEdit(nil), f.edits...)
}

type fakeRenderer struct {
	mu    sync.Mutex
	ups   map[string]card.Update
	ok    bool
	calls map[string]int
	gone  map[string]bool
	errs  map[string]error
}

func newRenderer(ok bool) *fakeRenderer {
	return &fakeRenderer{
		ups: map[string]card.Update{}, ok: ok, calls: map[string]int{}, gone: map[string]bool{},
		errs: map[string]error{},
	}
}

func (r *fakeRenderer) render(_ context.Context, subject string) (card.Rendering, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[subject]++
	if err := r.errs[subject]; err != nil {
		return card.Rendering{}, err
	}
	if r.gone[subject] {
		return card.Rendering{Outcome: card.Gone}, nil
	}
	if _, set := r.ups[subject]; !set && !r.ok {
		return card.Rendering{}, nil
	}
	return card.Rendered(r.ups[subject]), nil
}

func (r *fakeRenderer) set(subject string, up card.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ups[subject] = up
	r.ok = true
}

func (r *fakeRenderer) setGone(subject string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gone[subject] = true
}

func (r *fakeRenderer) setErr(subject string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs[subject] = err
}

func (r *fakeRenderer) count(subject string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[subject]
}

type cardEnv struct {
	t        *testing.T
	path     string
	public   string
	api      *editSender
	bot      *discord.Bot
	db       *db.DB
	events   chan *db.Event
	handlers map[string]card.Handler
}

func openCardDB(t *testing.T) *db.DB {
	t.Helper()
	d, _ := openCardDBAt(t)
	return d
}

func openCardDBAt(t *testing.T) (*db.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cards.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, path
}

func newCardEnv(t *testing.T, publicURL string) *cardEnv {
	t.Helper()
	return newCardEnvLog(t, publicURL, slog.New(slog.DiscardHandler))
}

func newCardEnvLog(t *testing.T, publicURL string, log *slog.Logger) *cardEnv {
	t.Helper()
	d, path := openCardDBAt(t)
	api := &editSender{}
	b := discord.NewWithSenderForTest(log, api, "owner", publicURL)
	t.Cleanup(func() { _ = b.Close() })
	return &cardEnv{
		t: t, path: path, public: publicURL, api: api, bot: b, db: d, events: make(chan *db.Event, 8),
	}
}

func (e *cardEnv) start(renderers map[string]card.Renderer) {
	e.bot.StartCards(e.t.Context(), e.db, e.events, e.handlers, renderers)
}

func (e *cardEnv) track(subject, messageID string, c card.Card, sentAt time.Time) {
	e.t.Helper()
	e.trackRows(messageID, c, sentAt, subject)
}

func (e *cardEnv) trackRows(messageID string, c card.Card, sentAt time.Time, subjects ...string) {
	e.t.Helper()
	body, err := json.Marshal(c)
	if err != nil {
		e.t.Fatal(err)
	}
	hash := e.hashOf(discord.RenderForTest(c, e.public))
	rows := make([]db.DiscordCard, 0, len(subjects))
	for _, s := range subjects {
		rows = append(rows, db.DiscordCard{
			Subject: s, ChannelID: "dm", MessageID: messageID, Card: body, Hash: hash, SentAt: sentAt,
		})
	}
	if err = e.db.InsertDiscordCards(e.t.Context(), rows); err != nil {
		e.t.Fatal(err)
	}
}

func (e *cardEnv) hashOf(comps []discordgo.MessageComponent) string {
	e.t.Helper()
	j, err := json.Marshal(comps)
	if err != nil {
		e.t.Fatal(err)
	}
	return card.Fingerprint(j)
}

func (e *cardEnv) rawDB() *sql.DB {
	e.t.Helper()
	raw, err := sql.Open("sqlite", e.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = raw.Close() })
	return raw
}

func (e *cardEnv) rawExec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.rawDB().ExecContext(e.t.Context(), query, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *cardEnv) rawCount(query string) int {
	e.t.Helper()
	var n int
	if err := e.rawDB().QueryRowContext(e.t.Context(), query).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *cardEnv) setLast(subject string, value any) {
	e.t.Helper()
	e.rawExec(
		`UPDATE discord_cards SET last_update = ? WHERE message_id = ? AND subject = ?`,
		value,
		"m1",
		subject,
	)
}

func (e *cardEnv) setHash(hash string) {
	e.t.Helper()
	e.rawExec(`UPDATE discord_cards SET hash = ? WHERE message_id = ?`, hash, "m1")
}

func (e *cardEnv) row(subject string) db.DiscordCard {
	e.t.Helper()
	rows, err := e.db.ListDiscordMessage(e.t.Context(), "m1")
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rows {
		if r.Subject == subject {
			return r
		}
	}
	e.t.Fatalf("no row %s", subject)
	return db.DiscordCard{}
}

func (e *cardEnv) waitLast(want string) {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for string(e.row("inc:1").LastUpdate) != want {
		if time.Now().After(deadline) {
			e.t.Fatalf("last_update = %s, want %s", e.row("inc:1").LastUpdate, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func (e *cardEnv) waitEdits(n int) []*discordgo.MessageEdit {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		if edits := e.api.editList(); len(edits) >= n {
			return edits
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("edits = %d, want %d", len(e.api.editList()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (e *cardEnv) waitCalls(r *fakeRenderer, subject string, n int) {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for r.count(subject) < n {
		if time.Now().After(deadline) {
			e.t.Fatalf("%s rendered %d times, want %d", subject, r.count(subject), n)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
}

func (e *cardEnv) event(seq int64, incidentID, kind string) {
	e.events <- &db.Event{Seq: seq, IncidentID: incidentID, Kind: kind}
}

func btn(kind, verb string, args ...string) card.Button {
	id, _ := card.Encode(card.Action{Kind: kind, Verb: verb, Args: args})
	return card.Button{Label: verb, Style: card.Primary, ID: id}
}

func editJSON(t *testing.T, m *discordgo.MessageEdit) string {
	t.Helper()
	b, err := json.Marshal(m.Components)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func incCard(id string) card.Card {
	return card.Card{Subject: card.IncidentSubject(id), Title: "Incident " + id, Verbs: []string{"resolve"}}
}

func TestNotifyOwner_RefreshesAfterRecord(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(true)
	r.set("inc:1", card.Update{Buttons: []card.Button{btn("inc", "resolve", "1", "s")}})
	e.start(map[string]card.Renderer{"inc": r.render})
	if err := e.bot.NotifyOwner(t.Context(), incCard("1")); err != nil {
		t.Fatal(err)
	}
	edits := e.waitEdits(1)
	if !strings.Contains(editJSON(t, edits[0]), "mf1:inc:resolve:1:s") || edits[0].ID != "sent1" {
		t.Errorf("edit = %s on %q", editJSON(t, edits[0]), edits[0].ID)
	}
	rows, err := e.db.ListDiscordSubject(t.Context(), "inc:1")
	if err != nil || len(rows) != 1 || rows[0].MessageID != "sent1" {
		t.Fatalf("rows %+v err %v", rows, err)
	}
}

func TestUpdater_StatusEventEdits(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(false)
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitCalls(r, "inc:1", 1)
	r.set("inc:1", card.Update{Buttons: []card.Button{btn("inc", "resolve", "1", "s")}})
	e.event(1, "1", "status_changed")
	edits := e.waitEdits(1)
	m := edits[0]
	if m.ID != "m9" || m.Channel != "dm" || m.Flags&discordgo.MessageFlagsIsComponentsV2 == 0 {
		t.Errorf("edit id %q channel %q flags %v", m.ID, m.Channel, m.Flags)
	}
	if !strings.Contains(editJSON(t, m), "mf1:inc:resolve:1:s") {
		t.Errorf("edit = %s", editJSON(t, m))
	}
}

func TestUpdater_LLMRoundNoEdit(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(false)
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitCalls(r, "inc:1", 1)
	r.set("inc:1", card.Update{Status: "new"})
	e.event(1, "1", "llm_round")
	time.Sleep(50 * time.Millisecond)
	if n := len(e.api.editList()); n != 0 || r.count("inc:1") != 1 {
		t.Errorf("edits %d, renders %d", n, r.count("inc:1"))
	}
}

func TestUpdater_UnchangedRenderNoEdit(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	e.bot.Refresh("inc:1")
	e.waitCalls(r, "inc:1", 2)
	e.bot.Refresh("inc:1")
	e.waitCalls(r, "inc:1", 3)
	if n := len(e.api.editList()); n != 1 {
		t.Fatalf("edits = %d, want 1", n)
	}
	if err := e.bot.NotifyOwner(t.Context(), card.Card{Subject: "inc:2", Title: "Plain"}); err != nil {
		t.Fatal(err)
	}
	e.waitCalls(r, "inc:2", 1)
	if n := len(e.api.editList()); n != 1 {
		t.Errorf("edits = %d after a send whose render did not change, want 1", n)
	}
}

func TestUpdater_SeqGapRefreshesAllInc(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(false)
	e.track("inc:1", "m1", incCard("1"), time.Now())
	e.track("inc:2", "m2", incCard("2"), time.Now())
	e.track("sq:s:abc", "m3", card.Card{Subject: "sq:s:abc"}, time.Now())
	e.start(map[string]card.Renderer{"inc": r.render, "sq": r.render})
	e.waitCalls(r, "sq:s:abc", 1)
	e.waitCalls(r, "inc:1", 1)
	e.waitCalls(r, "inc:2", 1)
	e.event(5, "1", "llm_round")
	e.event(7, "2", "llm_round")
	e.waitCalls(r, "inc:1", 2)
	e.waitCalls(r, "inc:2", 2)
	e.event(6, "1", "llm_round")
	time.Sleep(50 * time.Millisecond)
	if a, b, s := r.count("inc:1"), r.count("inc:2"), r.count("sq:s:abc"); a != 2 || b != 2 || s != 1 {
		t.Errorf("renders inc:1 %d inc:2 %d sq %d, want 2 2 1", a, b, s)
	}
}

func TestUpdater_404DropsRows(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.api.editErr = &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}}
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.track("inc:1", "m9", incCard("1"), time.Now())
	discord.SetOverlayForTest(e.bot, "m9", "Resolved")
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	deadline := time.Now().Add(waitLimit)
	for {
		rows, err := e.db.ListDiscordMessage(t.Context(), "m9")
		if err == nil && len(rows) == 0 && !discord.HasOverlayForTest(e.bot, "m9") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 404: rows %+v err %v overlay %v", rows, err, discord.HasOverlayForTest(e.bot, "m9"))
		}
		time.Sleep(time.Millisecond)
	}
	e.bot.Refresh("inc:1")
	time.Sleep(50 * time.Millisecond)
	if n := len(e.api.editList()); n != 1 {
		t.Errorf("edit attempts = %d, want 1", n)
	}
}

func TestUpdater_UnknownStateLeavesMessage(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(false)
	e.track("inc:1", "m1", incCard("1"), time.Now())
	e.track("zz:1", "m2", card.Card{Subject: "zz:1", Title: "no renderer"}, time.Now())
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitCalls(r, "inc:1", 1)
	e.bot.Refresh("zz:1")
	time.Sleep(50 * time.Millisecond)
	if n := len(e.api.editList()); n != 0 {
		t.Errorf("edits = %d, want 0", n)
	}
}

func TestUpdater_LivePolls(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	var (
		mu sync.Mutex
		n  int
	)
	live := func(context.Context, string) (card.Rendering, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return card.Rendered(card.Update{Status: fmt.Sprintf("probed %d", n), Live: true}), nil
	}
	discord.SetPollIntervalForTest(e.bot, 10*time.Millisecond)
	e.track("sweep:1", "m1", card.Card{Subject: "sweep:1", Title: "Sweep"}, time.Now())
	e.start(map[string]card.Renderer{"sweep": live})
	e.waitEdits(3)
}

func TestUpdater_StartupRefreshAndPrune(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	r.set("inc:2", card.Update{Status: "Escalated"})
	e.track("inc:1", "old", incCard("1"), time.Now().Add(-15*24*time.Hour))
	e.track("inc:2", "new", incCard("2"), time.Now().Add(-24*time.Hour))
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	time.Sleep(50 * time.Millisecond)
	edits := e.api.editList()
	if len(edits) != 1 || edits[0].ID != "new" || r.count("inc:2") != 1 || r.count("inc:1") != 0 {
		t.Errorf("edits %d (first %q), renders inc:1 %d inc:2 %d", len(edits), edits[0].ID, r.count("inc:1"),
			r.count("inc:2"))
	}
	if rows, err := e.db.ListDiscordMessage(t.Context(), "old"); err != nil || len(rows) != 0 {
		t.Errorf("old rows %+v err %v", rows, err)
	}
	if rows, err := e.db.ListDiscordMessage(t.Context(), "new"); err != nil || len(rows) != 1 {
		t.Errorf("new rows %+v err %v", rows, err)
	}
}

func TestUpdater_OverlayKeepsUntilUpdateChanges(t *testing.T) {
	t.Parallel()
	for _, overlayFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("overlayFirst=%v", overlayFirst), func(t *testing.T) {
			t.Parallel()
			e := newCardEnv(t, "")
			r := newRenderer(false)
			e.track("inc:1", "m9", incCard("1"), time.Now())
			e.start(map[string]card.Renderer{"inc": r.render})
			e.waitCalls(r, "inc:1", 1)
			r.set("inc:1", card.Update{Status: "Escalated"})
			want, calls := 1, 3
			if overlayFirst {
				discord.SetOverlayForTest(e.bot, "m9", "Resolve refused")
				e.waitEdits(1)
				e.event(1, "1", "status_changed")
			} else {
				e.event(1, "1", "status_changed")
				e.waitEdits(1)
				discord.SetOverlayForTest(e.bot, "m9", "Resolve refused")
				want, calls = 2, 4
				e.waitEdits(want)
				e.event(2, "1", "status_changed")
			}
			e.waitCalls(r, "inc:1", calls)
			edits := e.api.editList()
			if len(edits) != want || !strings.Contains(editJSON(t, edits[want-1]), "Resolve refused") {
				t.Fatalf("edits %d, last %s", len(edits), editJSON(t, edits[len(edits)-1]))
			}
			r.set("inc:1", card.Update{Status: "Resolved"})
			e.event(3, "1", "status_changed")
			last := e.waitEdits(want + 1)[want]
			if s := editJSON(t, last); strings.Contains(s, "Resolve refused") || !strings.Contains(s, "Resolved") {
				t.Errorf("edit after the change = %s", s)
			}
		})
	}
}

func TestNotifyOwner_PlainFallbackIsUntracked(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.api.complErr = &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusBadRequest}}
	e.start(map[string]card.Renderer{})
	err := e.bot.NotifyOwner(t.Context(), incCard("1"))
	if !errors.Is(err, card.ErrUntracked) {
		t.Errorf("err = %v", err)
	}
	if rows, lerr := e.db.ListDiscordCards(t.Context(), ""); lerr != nil || len(rows) != 0 {
		t.Errorf("rows %+v err %v", rows, lerr)
	}
}

func TestNotifyOwner_RecordFailureIsUntracked(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.start(map[string]card.Renderer{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := e.bot.NotifyOwner(ctx, incCard("1"))
	if !errors.Is(err, card.ErrUntracked) {
		t.Errorf("err = %v", err)
	}
	if e.api.sent != 1 {
		t.Errorf("sent = %d, want 1", e.api.sent)
	}
}

func TestNotifyUser_NeverRecords(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.start(map[string]card.Renderer{})
	if err := e.bot.NotifyUser(t.Context(), "reporter", incCard("1")); err != nil {
		t.Fatal(err)
	}
	if rows, err := e.db.ListDiscordCards(t.Context(), ""); err != nil || len(rows) != 0 {
		t.Errorf("rows %+v err %v", rows, err)
	}
}

func TestNotifyOwner_BeforeStartCardsUntracked(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	err := e.bot.NotifyOwner(t.Context(), incCard("1"))
	if !errors.Is(err, card.ErrUntracked) {
		t.Errorf("err = %v", err)
	}
	if e.api.sent != 1 {
		t.Errorf("sent = %d, want 1", e.api.sent)
	}
	if rows, lerr := e.db.ListDiscordCards(t.Context(), ""); lerr != nil || len(rows) != 0 {
		t.Errorf("rows %+v err %v", rows, lerr)
	}
}

func TestStartCards_ReturnsBeforeStartupRefresh(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.api.block = make(chan struct{})
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.track("inc:1", "m9", incCard("1"), time.Now())
	done := make(chan struct{})
	go func() {
		e.start(map[string]card.Renderer{"inc": r.render})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitLimit):
		t.Error("StartCards blocked on the startup refresh")
	}
	close(e.api.block)
}

func TestUpdater_ClosedEventsExits(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.start(map[string]card.Renderer{})
	close(e.events)
	select {
	case <-discord.CardsDoneForTest(e.bot):
	case <-time.After(waitLimit):
		t.Error("updater still running after the events channel closed")
	}
}

func countComponents(cs []discordgo.MessageComponent) int {
	n := 0
	for _, c := range cs {
		n++
		switch v := c.(type) {
		case discordgo.Container:
			n += countComponents(v.Components)
		case discordgo.ActionsRow:
			n += countComponents(v.Components)
		case discordgo.Section:
			n += countComponents(v.Components) + 1
		}
	}
	return n
}

func TestRender_LargestCardWithOverlayFits(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "https://h")
	verbs := []string{"a", "b", "c"}
	c := card.Card{Subject: "t:card", Title: "Big", Verbs: verbs, Path: "/x"}
	const items = 5
	for i := range items {
		s := fmt.Sprintf("t:i%d", i)
		it := card.Item{Subject: s, Text: s}
		for _, v := range verbs[:3-i/(items-1)] {
			it.Buttons = append(it.Buttons, btn("t", v, s[2:]))
		}
		c.Items = append(c.Items, it)
	}
	if c.Components() != card.MaxBuilt {
		t.Fatalf("built %d components, want %d", c.Components(), card.MaxBuilt)
	}
	more := func(arg string) []card.Button {
		return []card.Button{btn("t", "a", arg), btn("t", "b", arg), btn("t", "c", arg), btn("t", "d", arg)}
	}
	r := newRenderer(true)
	r.set("t:card", card.Update{
		Status: "running", Sections: []card.Field{{Name: "N", Value: "1"}}, Buttons: more("card"),
	})
	for i := range items {
		s := fmt.Sprintf("t:i%d", i)
		r.set(s, card.Update{Status: "still stuck", Buttons: more(s[2:])})
	}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	rows := []db.DiscordCard{{Subject: c.Subject, ChannelID: "dm", MessageID: "m1", Card: body, SentAt: time.Now()}}
	for _, it := range c.Items {
		rows = append(rows, db.DiscordCard{
			Subject: it.Subject, ChannelID: "dm", MessageID: "m1", Card: body, SentAt: time.Now(),
		})
	}
	if err = e.db.InsertDiscordCards(t.Context(), rows); err != nil {
		t.Fatal(err)
	}
	discord.SetOverlayForTest(e.bot, "m1", "Imported")
	e.start(map[string]card.Renderer{"t": r.render})
	m := e.waitEdits(1)[0]
	if n := countComponents(*m.Components); n > card.MaxComponents {
		t.Errorf("rendered %d components, limit %d", n, card.MaxComponents)
	}
	if s := editJSON(t, m); strings.Contains(s, "mf1:t:d:") || !strings.Contains(s, "Imported") {
		t.Errorf("edit = %s", s)
	}
}

func TestRefresh_OverTextLimitDropsSectionsAndDanger(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	c := incCard("1")
	c.Body = strings.Repeat("r", card.MaxText/2)
	c.Verbs = []string{"approve", "refresh"}
	approve := btn("inc", "approve", "1", "s", "fp")
	approve.Style = card.Danger
	r := newRenderer(true)
	r.set("inc:1", card.Update{
		Sections: []card.Field{{Name: "Delete", Value: strings.Repeat("p", card.MaxText/2)}},
		Buttons:  []card.Button{approve, btn("inc", "refresh", "1", "s")},
	})
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.db.InsertDiscordCards(t.Context(), []db.DiscordCard{
		{Subject: c.Subject, ChannelID: "dm", MessageID: "m1", Card: body, SentAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	e.start(map[string]card.Renderer{"inc": r.render})
	e.bot.Refresh("inc:1")
	m := e.waitEdits(1)[0]
	if n := len(texts(*m.Components)); n > card.MaxText {
		t.Errorf("edit text = %d chars, limit %d", n, card.MaxText)
	}
	s := editJSON(t, m)
	if strings.Contains(s, "mf1:inc:approve:") || !strings.Contains(s, "mf1:inc:refresh:") ||
		!strings.Contains(s, "too long for Discord") {
		t.Errorf("edit = %.300s…", s)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), sub)
}

func twoItemCard() card.Card {
	return card.Card{
		Subject: "t:card", Title: "Two",
		Items: []card.Item{{Subject: "r:1", Text: "first"}, {Subject: "s:1", Text: "second"}},
	}
}

func updateJSON(t *testing.T, u card.Update) string {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *cardEnv) waitNoRows(messageID string) {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		rows, err := e.db.ListDiscordMessage(e.t.Context(), messageID)
		if err == nil && len(rows) == 0 && !discord.HasOverlayForTest(e.bot, messageID) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("rows %+v err %v overlay %v", rows, err, discord.HasOverlayForTest(e.bot, messageID))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestD1_10_UnreachableAppKeepsOtherItemsUpdating(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.trackRows("m1", twoItemCard(), time.Now(), "t:card", "r:1", "s:1")
	e.setLast("r:1", updateJSON(t, card.Update{Status: "r-old-status"}))
	notYet := newRenderer(false)
	s := newRenderer(true)
	s.set("s:1", card.Update{Status: "s-new-status"})
	e.start(map[string]card.Renderer{"t": notYet.render, "r": notYet.render, "s": s.render})
	got := editJSON(t, e.waitEdits(1)[0])
	if !strings.Contains(got, "r-old-status") || !strings.Contains(got, "s-new-status") {
		t.Errorf("edit = %s", got)
	}
}

func TestD1_10_PrunedSweepRunIsForgotten(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(false)
	r.setGone("sweep:1")
	e.track("sweep:1", "m1", card.Card{Subject: "sweep:1", Title: "Sweep"}, time.Now())
	e.start(map[string]card.Renderer{"sweep": r.render})
	e.waitNoRows("m1")
	e.bot.Refresh("sweep:")
	time.Sleep(50 * time.Millisecond)
	if n := r.count("sweep:1"); n != 1 {
		t.Errorf("renders = %d, want 1", n)
	}
	if n := len(e.api.editList()); n != 0 {
		t.Errorf("edits = %d, want 0", n)
	}
}

func TestD1_10_MissingRendererFreezesOnlyItsSubject(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.trackRows("m1", twoItemCard(), time.Now(), "t:card", "r:1", "s:1")
	e.setLast("r:1", updateJSON(t, card.Update{Status: "r-old-status"}))
	notYet := newRenderer(false)
	s := newRenderer(true)
	s.set("s:1", card.Update{Status: "s-new-status"})
	e.start(map[string]card.Renderer{"t": notYet.render, "s": s.render})
	got := editJSON(t, e.waitEdits(1)[0])
	if !strings.Contains(got, "r-old-status") || !strings.Contains(got, "s-new-status") {
		t.Errorf("edit = %s", got)
	}
}

func TestD1_10_NotYetAfterRestartKeepsStoredUpdate(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.track("inc:1", "m1", incCard("1"), time.Now())
	up := card.Update{Status: "Escalated"}
	e.setLast("inc:1", updateJSON(t, up))
	e.setHash("stale")
	r := newRenderer(false)
	e.start(map[string]card.Renderer{"inc": r.render})
	if got := editJSON(t, e.waitEdits(1)[0]); !strings.Contains(got, "Escalated") {
		t.Errorf("edit = %s", got)
	}
	if row := e.row("inc:1"); string(row.LastUpdate) != updateJSON(t, up) {
		t.Errorf("last_update = %s", row.LastUpdate)
	}
}

func TestD1_10_NullLastNotYetSkipsEdit(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.track("inc:1", "m1", incCard("1"), time.Now())
	e.setLast("inc:1", nil)
	e.setHash("edited-before-p5")
	r := newRenderer(false)
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitCalls(r, "inc:1", 1)
	if row := e.row("inc:1"); len(e.api.editList()) != 0 || row.LastKnown || row.Hash != "edited-before-p5" {
		t.Fatalf("edits %d, row %+v", len(e.api.editList()), row)
	}
	up := card.Update{Status: "Escalated"}
	r.set("inc:1", up)
	e.bot.Refresh("inc:1")
	e.waitEdits(1)
	e.waitLast(updateJSON(t, up))
}

func TestD1_10_NullLastWithAsSentHashRendersAsSent(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.track("inc:1", "m1", incCard("1"), time.Now())
	e.setLast("inc:1", nil)
	r := newRenderer(false)
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitCalls(r, "inc:1", 1)
	e.waitLast("null")
	if n := len(e.api.editList()); n != 0 {
		t.Errorf("edits = %d, want 0", n)
	}
}

func TestD1_10_RenderErrorKeepsLast(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.track("inc:1", "m1", incCard("1"), time.Now())
	up := card.Update{Status: "kept-status"}
	e.setLast("inc:1", updateJSON(t, up))
	e.setHash("stale")
	r := newRenderer(false)
	r.setErr("inc:1", errors.New("probe failed"))
	e.start(map[string]card.Renderer{"inc": r.render})
	if got := editJSON(t, e.waitEdits(1)[0]); !strings.Contains(got, "kept-status") {
		t.Errorf("edit = %s", got)
	}
	if row := e.row("inc:1"); string(row.LastUpdate) != updateJSON(t, up) {
		t.Errorf("last_update = %s", row.LastUpdate)
	}
}

func TestD1_10_UndecodableLastWarnsOncePerMessage(t *testing.T) {
	t.Parallel()
	buf := &lockedBuf{}
	e := newCardEnvLog(t, "", slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	e.track("inc:1", "m1", incCard("1"), time.Now())
	e.setLast("inc:1", `"junk"`)
	e.setHash("stale")
	r := newRenderer(false)
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitCalls(r, "inc:1", 1)
	for i := 2; i <= 4; i++ {
		e.bot.Refresh("inc:1")
		e.waitCalls(r, "inc:1", i)
	}
	if n := buf.count("last_update"); n != 1 {
		t.Errorf("last_update warnings = %d, want 1", n)
	}
	if n := len(e.api.editList()); n != 0 {
		t.Errorf("edits = %d, want 0", n)
	}
	fresh := card.Update{Status: "fresh"}
	r.set("inc:1", fresh)
	e.bot.Refresh("inc:1")
	e.waitEdits(1)
	e.waitLast(updateJSON(t, fresh))
}

func TestD2_20_OverlayShownWhileASubjectIsNotYet(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.track("inc:1", "m1", incCard("1"), time.Now())
	r := newRenderer(false)
	discord.SetOverlayForTest(e.bot, "m1", "Resolve refused")
	e.start(map[string]card.Renderer{"inc": r.render})
	if got := editJSON(t, e.waitEdits(1)[0]); !strings.Contains(got, "Resolve refused") {
		t.Errorf("edit = %s", got)
	}
}

func TestUpdater_GoneSubjectNeverRenderedAgain(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.trackRows("m1", twoItemCard(), time.Now(), "t:card", "r:1", "s:1")
	e.setLast("r:1", updateJSON(t, card.Update{Status: "r-last-status"}))
	notYet := newRenderer(false)
	r := newRenderer(false)
	r.setGone("r:1")
	s := newRenderer(true)
	s.set("s:1", card.Update{Status: "s-new-status"})
	e.start(map[string]card.Renderer{"t": notYet.render, "r": r.render, "s": s.render})
	got := editJSON(t, e.waitEdits(1)[0])
	if !strings.Contains(got, "r-last-status") || !strings.Contains(got, "s-new-status") {
		t.Errorf("edit = %s", got)
	}
	if st := e.row("r:1").State; st != db.CardGone {
		t.Errorf("r:1 state = %q", st)
	}
	s.set("s:1", card.Update{Status: "s-newer-status"})
	e.bot.Refresh("s:1")
	if got = editJSON(t, e.waitEdits(2)[1]); !strings.Contains(got, "r-last-status") ||
		!strings.Contains(got, "s-newer-status") {
		t.Errorf("second edit = %s", got)
	}
	if n := r.count("r:1"); n != 1 {
		t.Errorf("r:1 rendered %d times, want 1", n)
	}
}

func TestUpdater_AllGoneSkipsEditEvenWithOverlay(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.track("inc:1", "m1", incCard("1"), time.Now())
	r := newRenderer(false)
	r.setGone("inc:1")
	discord.SetOverlayForTest(e.bot, "m1", "Resolved")
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitNoRows("m1")
	if n := len(e.api.editList()); n != 0 {
		t.Errorf("edits = %d, want 0", n)
	}
}

func TestUpdater_StoresLastUpdateOnlyWhenChanged(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	a := card.Update{Status: "A"}
	r := newRenderer(true)
	r.set("inc:1", a)
	e.track("inc:1", "m1", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	e.waitLast(updateJSON(t, a))
	e.rawExec(`CREATE TABLE writes (n INTEGER)`)
	e.rawExec(`CREATE TRIGGER count_writes AFTER UPDATE ON discord_cards BEGIN INSERT INTO writes VALUES (1); END`)
	e.bot.Refresh("inc:1")
	e.waitCalls(r, "inc:1", 2)
	if n := e.rawCount(`SELECT count(*) FROM writes`); n != 0 {
		t.Errorf("writes after an unchanged render = %d, want 0", n)
	}
	bb := card.Update{Status: "B"}
	r.set("inc:1", bb)
	e.bot.Refresh("inc:1")
	e.waitEdits(2)
	e.waitLast(updateJSON(t, bb))
}

func logEnv(t *testing.T) (*cardEnv, *lockedBuf) {
	t.Helper()
	buf := &lockedBuf{}
	return newCardEnvLog(t, "", slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))), buf
}

func serverError() error {
	return &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusInternalServerError}}
}

func rateLimited(wait time.Duration) error {
	return &discordgo.RateLimitError{
		RateLimit: &discordgo.RateLimit{TooManyRequests: &discordgo.TooManyRequests{RetryAfter: wait}},
	}
}

func (e *cardEnv) trackInc(messageID, subject string) {
	e.t.Helper()
	e.track(subject, messageID, incCard(strings.TrimPrefix(subject, "inc:")), time.Now())
}

func TestD1_9_UndecodableCardIsDropped(t *testing.T) {
	t.Parallel()
	e, buf := logEnv(t)
	e.track("inc:1", "m1", incCard("1"), time.Now())
	e.rawExec("UPDATE discord_cards SET card = ?", "[1]")
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitNoRows("m1")
	e.bot.Refresh("inc:1")
	time.Sleep(50 * time.Millisecond)
	if n := buf.count("level=WARN"); n != 1 {
		t.Errorf("warnings = %d, want 1", n)
	}
	if n := len(e.api.editList()); n != 0 {
		t.Errorf("edits = %d, want 0", n)
	}
}

func TestD1_13_FailedEditRetriesWithBackoff(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.api.errQueue = []error{serverError()}
	discord.SetCardRetryForTest(e.bot, 200*time.Millisecond, time.Hour)
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.trackInc("m1", "inc:1")
	began := time.Now()
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.api.editList()); n != 1 {
		t.Fatalf("edits before the retry window = %d, want 1", n)
	}
	e.waitEdits(2)
	if took := time.Since(began); took < 200*time.Millisecond {
		t.Errorf("retry after %v, want at least 200ms", took)
	}
	e.api.emu.Lock()
	defer e.api.emu.Unlock()
	if slices.Contains(e.api.retried, true) {
		t.Errorf("edits left discordgo's own 429 retry on: %v", e.api.retried)
	}
}

func TestD1_13_RateLimitWaitsRetryAfter(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.api.errQueue = []error{rateLimited(400 * time.Millisecond)}
	discord.SetCardRetryForTest(e.bot, 50*time.Millisecond, time.Hour)
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.trackInc("m1", "inc:1")
	began := time.Now()
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	time.Sleep(100 * time.Millisecond)
	discord.SetOverlayForTest(e.bot, "m1", "Pressed")
	time.Sleep(100 * time.Millisecond)
	if n := len(e.api.editList()); n != 1 {
		t.Fatalf("edits inside Retry-After = %d, want 1", n)
	}
	e.waitEdits(2)
	if took := time.Since(began); took < 400*time.Millisecond {
		t.Errorf("second edit after %v, want at least 400ms", took)
	}
}

func TestD1_13_SystemTriggerWaitsForRetry(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.api.errQueue = []error{serverError()}
	discord.SetCardRetryForTest(e.bot, 400*time.Millisecond, time.Hour)
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.trackInc("m1", "inc:1")
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	r.set("inc:1", card.Update{Status: "Resolved"})
	e.bot.Refresh("inc:1")
	time.Sleep(200 * time.Millisecond)
	if n := len(e.api.editList()); n != 1 {
		t.Fatalf("edits after a system trigger during backoff = %d, want 1", n)
	}
	e.waitEdits(2)
}

func TestD1_13_PressDuringBackoffEditsNow(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	e.api.errQueue = []error{serverError()}
	discord.SetCardRetryForTest(e.bot, time.Hour, time.Hour)
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.trackInc("m1", "inc:1")
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	discord.SetOverlayForTest(e.bot, "m1", "Pressed")
	e.waitEdits(2)
}

func TestD1_13_OneWarnPerEpisode(t *testing.T) {
	t.Parallel()
	e, buf := logEnv(t)
	e.api.errQueue = []error{serverError(), serverError(), serverError()}
	discord.SetCardRetryForTest(e.bot, 50*time.Millisecond, 100*time.Millisecond)
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.trackInc("m1", "inc:1")
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(4)
	deadline := time.Now().Add(waitLimit)
	for buf.count(`level=INFO msg="discord card edit recovered"`) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("no recovered log line")
		}
		time.Sleep(time.Millisecond)
	}
	if n := buf.count(`level=WARN msg="discord card edit failed"`); n != 1 {
		t.Errorf("failed warnings = %d, want 1", n)
	}
	if n := buf.count(`level=INFO msg="discord card edit failed"`); n != 2 {
		t.Errorf("failed infos = %d, want 2", n)
	}
	if n := buf.count("level=WARN"); n != 1 {
		t.Errorf("warnings = %d, want 1", n)
	}
}

func TestUpdater_PressAndRefreshAreOneAttempt(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	e.trackInc("m1", "inc:1")
	discord.SetOverlayForTest(e.bot, "m1", "Pressed")
	e.bot.Refresh("inc:1")
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.api.editList()); n != 1 {
		t.Errorf("edits = %d, want 1", n)
	}
	if n := r.count("inc:1"); n != 1 {
		t.Errorf("render calls = %d, want 1", n)
	}
}

func TestUpdater_PollSetIsLiveMessages(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	counts := map[string]*atomic.Int32{"sweep:1": {}, "sweep:2": {}, "sweep:3": {}}
	renderer := func(live bool) card.Renderer {
		return func(_ context.Context, subject string) (card.Rendering, error) {
			n := counts[subject].Add(1)
			return card.Rendered(card.Update{Status: fmt.Sprintf("probe %d", n), Live: live}), nil
		}
	}
	e.api.errFor = map[string]error{"m3": serverError()}
	discord.SetPollIntervalForTest(e.bot, 10*time.Millisecond)
	discord.SetCardRetryForTest(e.bot, time.Hour, time.Hour)
	for i, subject := range []string{"sweep:1", "sweep:2", "sweep:3"} {
		e.track(subject, fmt.Sprintf("m%d", i+1), card.Card{Subject: subject, Title: "Sweep"}, time.Now())
	}
	e.start(map[string]card.Renderer{"sweep": func(ctx context.Context, subject string) (card.Rendering, error) {
		return renderer(subject != "sweep:2")(ctx, subject)
	}})
	deadline := time.Now().Add(waitLimit)
	for counts["sweep:1"].Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatal("the live message was not polled")
		}
		time.Sleep(time.Millisecond)
	}
	if n := counts["sweep:2"].Load(); n != 1 {
		t.Errorf("not-live message rendered %d times, want 1", n)
	}
	if n := counts["sweep:3"].Load(); n != 1 {
		t.Errorf("failing message rendered %d times, want 1", n)
	}
}

func TestUpdater_ShutdownMidPassStopsWithoutWarn(t *testing.T) {
	t.Parallel()
	e, buf := logEnv(t)
	entered := make(chan struct{})
	var calls atomic.Int32
	e.api.hold = func(ctx context.Context, _ string) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitLimit):
			return errors.New("edit context never cancelled")
		}
	}
	r := newRenderer(true)
	r.set("inc:1", card.Update{Status: "Escalated"})
	r.set("inc:2", card.Update{Status: "Escalated"})
	e.trackInc("m1", "inc:1")
	e.trackInc("m2", "inc:2")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.bot.StartCards(ctx, e.db, e.events, e.handlers, map[string]card.Renderer{"inc": r.render})
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("no edit started")
	}
	cancel()
	select {
	case <-discord.CardsDoneForTest(e.bot):
	case <-time.After(waitLimit):
		t.Fatal("cardsDone did not close")
	}
	if n := buf.count("level=WARN"); n != 0 {
		t.Errorf("warnings = %d, want 0", n)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("edit attempts = %d, want 1", n)
	}
}

func TestUpdater_RefreshNeverBlocksDuringPass(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "")
	entered, release := make(chan struct{}), make(chan struct{})
	closeRelease := sync.OnceFunc(func() { close(release) })
	t.Cleanup(closeRelease)
	var once sync.Once
	e.api.hold = func(context.Context, string) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	var calls atomic.Int32
	changing := func(context.Context, string) (card.Rendering, error) {
		return card.Rendered(card.Update{Status: fmt.Sprintf("status %d", calls.Add(1))}), nil
	}
	passes := make(chan struct{}, 1000)
	discord.SetPollIntervalForTest(e.bot, time.Hour)
	discord.SetPassDoneForTest(e.bot, func() { passes <- struct{}{} })
	for i := 1; i <= 3; i++ {
		e.trackInc(fmt.Sprintf("m%d", i), fmt.Sprintf("inc:%d", i))
	}
	e.start(map[string]card.Renderer{"inc": changing})
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("no edit started")
	}
	returned := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Go(func() {
				for i := range 125 {
					e.bot.Refresh(fmt.Sprintf("inc:%d", (g+i)%3+1))
				}
			})
		}
		wg.Wait()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(waitLimit):
		t.Fatal("Refresh blocked behind the pass")
	}
	closeRelease()
	for range 2 {
		select {
		case <-passes:
		case <-time.After(waitLimit):
			t.Fatal("pass did not finish")
		}
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("m%d", i)
		if n := e.editsOf(id); n < 1 || n > 2 {
			t.Errorf("%s edits = %d, want 1 or 2", id, n)
		}
	}
}

func (e *cardEnv) editsOf(messageID string) int {
	n := 0
	for _, ed := range e.api.editList() {
		if ed.ID == messageID {
			n++
		}
	}
	return n
}
