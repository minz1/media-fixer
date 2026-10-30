package discord_test

import (
	"context"
	"errors"
	"log/slog"
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

const (
	msgNotOwner = "Only the owner can use these buttons"
	msgOldID    = "This button is from an older version; use the dashboard link."
)

type clickSender struct {
	editSender

	rmu       sync.Mutex
	responds  []*discordgo.InteractionResponse
	followups []string
	respErr   error
}

func (f *clickSender) InteractionRespond(
	_ *discordgo.Interaction, r *discordgo.InteractionResponse, _ ...discordgo.RequestOption,
) error {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.responds = append(f.responds, r)
	return f.respErr
}

func (f *clickSender) InteractionResponseEdit(
	_ *discordgo.Interaction, w *discordgo.WebhookEdit, _ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if w.Content != nil {
		f.followups = append(f.followups, *w.Content)
	}
	return &discordgo.Message{}, nil
}

func (f *clickSender) respondList() []*discordgo.InteractionResponse {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return append([]*discordgo.InteractionResponse(nil), f.responds...)
}

func (f *clickSender) setRespErr(err error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.respErr = err
}

type clickEnv struct {
	*cardEnv

	click *clickSender
}

func newClickEnv(t *testing.T, ownerID string) *clickEnv {
	t.Helper()
	d := openCardDB(t)
	api := &clickSender{}
	b := discord.NewWithSenderForTest(slog.New(slog.DiscardHandler), api, ownerID, "")
	t.Cleanup(func() { _ = b.Close() })
	return &clickEnv{
		cardEnv: &cardEnv{
			t: t, api: &api.editSender, bot: b, db: d, events: make(chan *db.Event, 8),
			handlers: map[string]card.Handler{},
		},
		click: api,
	}
}

func dmClick(userID, messageID, customID string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:    discordgo.InteractionMessageComponent,
		Data:    discordgo.MessageComponentInteractionData{CustomID: customID},
		User:    &discordgo.User{ID: userID},
		Message: &discordgo.Message{ID: messageID, ChannelID: "dm"},
	}}
}

func guildClick(userID, messageID, customID string) *discordgo.InteractionCreate {
	i := dmClick(userID, messageID, customID)
	i.User = nil
	i.Member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	return i
}

func (e *clickEnv) press(i *discordgo.InteractionCreate) {
	discord.OnInteractionForTest(e.bot, i)
}

func (e *clickEnv) waitLastEdit(want func(string) bool) {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		if edits := e.api.editList(); len(edits) > 0 && want(editJSON(e.t, edits[len(edits)-1])) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no matching edit; edits = %d", len(e.api.editList()))
		}
		time.Sleep(time.Millisecond)
	}
}

func ephemeralContent(t *testing.T, r *discordgo.InteractionResponse) string {
	t.Helper()
	if r.Type != discordgo.InteractionResponseChannelMessageWithSource || r.Data == nil ||
		r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("response %+v is not an ephemeral message", r)
	}
	return r.Data.Content
}

func countingHandler(calls *atomic.Int32, res card.Result) card.Handler {
	return func(context.Context, card.Action) (card.Result, error) {
		calls.Add(1)
		return res, nil
	}
}

func TestClick_NonOwnerRefused(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	var calls atomic.Int32
	e.handlers["inc"] = countingHandler(&calls, card.Result{Line: "ran"})
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	id := btn("inc", "resolve", "1", "s").ID
	e.press(dmClick("stranger", "m9", id))
	e.press(guildClick("stranger", "m9", id))
	rs := e.click.respondList()
	if len(rs) != 2 {
		t.Fatalf("responses = %d, want 2", len(rs))
	}
	for _, r := range rs {
		if got := ephemeralContent(t, r); got != msgNotOwner {
			t.Errorf("content = %q", got)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("handler ran %d times", calls.Load())
	}
}

func TestClick_EmptyOwnerRefusesAll(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "")
	var calls atomic.Int32
	e.handlers["inc"] = countingHandler(&calls, card.Result{Line: "ran"})
	e.start(nil)
	id := btn("inc", "resolve", "1", "s").ID
	e.press(dmClick("", "m9", id))
	e.press(dmClick("owner", "m9", id))
	rs := e.click.respondList()
	if len(rs) != 2 {
		t.Fatalf("responses = %d, want 2", len(rs))
	}
	for _, r := range rs {
		if got := ephemeralContent(t, r); got != msgNotOwner {
			t.Errorf("content = %q", got)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("handler ran %d times", calls.Load())
	}
}

func TestClick_UnknownIDEphemeral(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	var calls atomic.Int32
	e.handlers["inc"] = countingHandler(&calls, card.Result{Line: "ran"})
	e.start(nil)
	e.press(dmClick("owner", "m9", "mf0:inc:resolve:1"))
	e.press(dmClick("owner", "m9", "mf1:zz:x:y"))
	rs := e.click.respondList()
	if len(rs) != 2 {
		t.Fatalf("responses = %d, want 2", len(rs))
	}
	for _, r := range rs {
		if got := ephemeralContent(t, r); got != msgOldID {
			t.Errorf("content = %q", got)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("handler ran %d times", calls.Load())
	}
}

func TestClick_AckFailsHandlerNotRun(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	var calls atomic.Int32
	e.handlers["inc"] = countingHandler(&calls, card.Result{Line: "ran"})
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	e.click.setRespErr(errors.New("unknown interaction"))
	id := btn("inc", "resolve", "1", "s").ID
	e.press(dmClick("owner", "m9", id))
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times after a failed ack", calls.Load())
	}
	e.click.setRespErr(nil)
	e.press(dmClick("owner", "m9", id))
	if calls.Load() != 1 {
		t.Errorf("retry ran the handler %d times, want 1", calls.Load())
	}
}

func TestClick_InFlightSecondClickRefused(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e.handlers["inc"] = func(context.Context, card.Action) (card.Result, error) {
		calls.Add(1)
		close(started)
		<-release
		return card.Result{Line: "done"}, nil
	}
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	done := make(chan struct{})
	go func() {
		e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
		close(done)
	}()
	<-started
	e.press(dmClick("owner", "m9", btn("inc", "rerun", "1", "s").ID))
	close(release)
	<-done
	rs := e.click.respondList()
	if len(rs) != 2 {
		t.Fatalf("responses = %d, want 2", len(rs))
	}
	if got := ephemeralContent(t, rs[1]); !strings.Contains(got, "already running") {
		t.Errorf("second click content = %q", got)
	}
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times", calls.Load())
	}
}

func TestClick_ResultBecomesOverlay(t *testing.T) {
	t.Parallel()
	for _, eventFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "event first"}[eventFirst], func(t *testing.T) {
			t.Parallel()
			e := newClickEnv(t, "owner")
			r := newRenderer(true)
			r.set("inc:1", card.Update{Status: "Needs you", Buttons: []card.Button{btn("inc", "resolve", "1", "s")}})
			e.handlers["inc"] = func(context.Context, card.Action) (card.Result, error) {
				if eventFirst {
					r.set("inc:1", card.Update{Status: "Resolved"})
					e.event(1, "1", "status_changed")
				}
				return card.Result{Line: "✅ Removed"}, nil
			}
			e.track("inc:1", "m9", incCard("1"), time.Now())
			e.start(map[string]card.Renderer{"inc": r.render})
			e.waitEdits(1)
			e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
			if rs := e.click.respondList(); len(rs) != 1 ||
				rs[0].Type != discordgo.InteractionResponseDeferredMessageUpdate {
				t.Fatalf("ack = %+v", rs)
			}
			e.waitLastEdit(func(s string) bool {
				return strings.Contains(s, "✅ Removed") && (!eventFirst || strings.Contains(s, "Resolved"))
			})
		})
	}
}

func TestClick_UntrackedMessage(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	e.handlers["inc"] = func(context.Context, card.Action) (card.Result, error) {
		return card.Result{Line: "✅ Removed"}, nil
	}
	e.start(nil)
	e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
	rs := e.click.respondList()
	if len(rs) != 1 || rs[0].Type != discordgo.InteractionResponseDeferredChannelMessageWithSource ||
		rs[0].Data == nil || rs[0].Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("ack = %+v", rs)
	}
	e.click.rmu.Lock()
	followups := append([]string(nil), e.click.followups...)
	e.click.rmu.Unlock()
	if len(followups) != 1 || followups[0] != "✅ Removed" {
		t.Errorf("follow-ups = %q", followups)
	}
	if discord.HasOverlayForTest(e.bot, "m9") {
		t.Error("overlay kept for an untracked message")
	}
	if n := len(e.api.editList()); n != 0 {
		t.Errorf("edits = %d, want 0", n)
	}
}

func TestClick_ConfirmFlow(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	var now atomic.Int64
	now.Store(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC).Unix())
	discord.SetClockForTest(e.bot, func() time.Time { return time.Unix(now.Load(), 0) })
	approve, confirm, cancel := btn("inc", "approve", "1", "s", "fp"), btn("inc", "confirm", "1", "fp"),
		btn("inc", "cancel", "1")
	r := newRenderer(true)
	r.set("inc:1", card.Update{Buttons: []card.Button{approve}})
	var confirms atomic.Int32
	e.handlers["inc"] = func(_ context.Context, a card.Action) (card.Result, error) {
		switch a.Verb {
		case "approve":
			return card.Result{Confirm: []card.Button{confirm, cancel}}, nil
		case "confirm":
			confirms.Add(1)
			return card.Result{Line: "Approved"}, nil
		}
		return card.Result{}, nil
	}
	c := incCard("1")
	c.Verbs = []string{"approve"}
	e.track("inc:1", "m9", c, time.Now())
	e.start(map[string]card.Renderer{"inc": r.render})
	e.waitEdits(1)
	showsConfirm := func(s string) bool { return strings.Contains(s, confirm.ID) && !strings.Contains(s, approve.ID) }
	reverted := func(s string) bool {
		return strings.Contains(s, approve.ID) && !strings.Contains(s, confirm.ID) && strings.Contains(s, "Refused")
	}

	e.press(dmClick("owner", "m9", confirm.ID))
	e.waitLastEdit(reverted)
	if confirms.Load() != 0 {
		t.Fatalf("confirm ran with no overlay")
	}

	e.press(dmClick("owner", "m9", approve.ID))
	e.waitLastEdit(showsConfirm)
	now.Add(int64((time.Minute).Seconds()))
	e.press(dmClick("owner", "m9", confirm.ID))
	e.waitLastEdit(func(s string) bool { return strings.Contains(s, "Approved") })
	if confirms.Load() != 1 {
		t.Fatalf("confirms = %d, want 1", confirms.Load())
	}

	e.press(dmClick("owner", "m9", approve.ID))
	e.waitLastEdit(showsConfirm)
	now.Add(int64((3 * time.Minute).Seconds()))
	e.press(dmClick("owner", "m9", confirm.ID))
	e.waitLastEdit(reverted)
	if confirms.Load() != 1 {
		t.Fatalf("an expired confirm ran: confirms = %d", confirms.Load())
	}

	e.press(dmClick("owner", "m9", approve.ID))
	e.waitLastEdit(showsConfirm)
	e.press(dmClick("owner", "m9", cancel.ID))
	e.waitLastEdit(func(s string) bool {
		return strings.Contains(s, approve.ID) && !strings.Contains(s, confirm.ID) && !strings.Contains(s, "Refused")
	})
	if discord.HasOverlayForTest(e.bot, "m9") {
		t.Error("Cancel kept the overlay")
	}
}

func TestClose_WaitsForHandlers(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	started, release := make(chan struct{}), make(chan struct{})
	e.handlers["inc"] = func(context.Context, card.Action) (card.Result, error) {
		close(started)
		<-release
		return card.Result{Line: "done"}, nil
	}
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	go e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
	<-started
	closed := make(chan struct{})
	go func() {
		_ = e.bot.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a handler was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(waitLimit):
		t.Fatal("Close did not return after the handler finished")
	}
}

func TestClose_BeforeStartCards(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	closed := make(chan struct{})
	go func() {
		_ = e.bot.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(waitLimit):
		t.Fatal("Close blocked without StartCards")
	}
}
