package discord_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
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
	m *discordgo.MessageEdit, _ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	if f.block != nil {
		<-f.block
	}
	f.emu.Lock()
	defer f.emu.Unlock()
	f.edits = append(f.edits, m)
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
}

func newRenderer(ok bool) *fakeRenderer {
	return &fakeRenderer{ups: map[string]card.Update{}, ok: ok, calls: map[string]int{}}
}

func (r *fakeRenderer) render(_ context.Context, subject string) (card.Update, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[subject]++
	return r.ups[subject], r.ok, nil
}

func (r *fakeRenderer) set(subject string, up card.Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ups[subject] = up
	r.ok = true
}

func (r *fakeRenderer) count(subject string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[subject]
}

type cardEnv struct {
	t        *testing.T
	api      *editSender
	bot      *discord.Bot
	db       *db.DB
	events   chan *db.Event
	handlers map[string]card.Handler
}

func openCardDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "cards.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func newCardEnv(t *testing.T, publicURL string) *cardEnv {
	t.Helper()
	d := openCardDB(t)
	api := &editSender{}
	b := discord.NewWithSenderForTest(slog.New(slog.DiscardHandler), api, "owner", publicURL)
	t.Cleanup(func() { _ = b.Close() })
	return &cardEnv{t: t, api: api, bot: b, db: d, events: make(chan *db.Event, 8)}
}

func (e *cardEnv) start(renderers map[string]card.Renderer) {
	e.bot.StartCards(e.t.Context(), e.db, e.events, e.handlers, renderers)
}

func (e *cardEnv) track(subject, messageID string, c card.Card, sentAt time.Time) {
	e.t.Helper()
	body, err := json.Marshal(c)
	if err != nil {
		e.t.Fatal(err)
	}
	if err = e.db.InsertDiscordCards(e.t.Context(), []db.DiscordCard{{
		Subject: subject, ChannelID: "dm", MessageID: messageID, Card: body, SentAt: sentAt,
	}}); err != nil {
		e.t.Fatal(err)
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
	live := func(context.Context, string) (card.Update, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return card.Update{Status: fmt.Sprintf("probed %d", n), Live: true}, true, nil
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
		}
	}
	return n
}

func TestRender_LargestCardWithOverlayFits(t *testing.T) {
	t.Parallel()
	e := newCardEnv(t, "https://h")
	verbs := []string{"a", "b", "c"}
	c := card.Card{Subject: "t:card", Title: "Big", Verbs: verbs, Path: "/x"}
	const items = 6
	for i := range items {
		s := fmt.Sprintf("t:i%d", i)
		it := card.Item{Subject: s, Text: s}
		for _, v := range verbs {
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
