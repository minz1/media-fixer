package discord_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/incident"
)

type followSender struct {
	clickSender

	fmu       sync.Mutex
	follows   []*discordgo.WebhookParams
	deletions int
	onDefer   func()
}

func (f *followSender) InteractionRespond(
	i *discordgo.Interaction, r *discordgo.InteractionResponse, o ...discordgo.RequestOption,
) error {
	if f.onDefer != nil && r.Type == discordgo.InteractionResponseDeferredChannelMessageWithSource {
		f.onDefer()
	}
	return f.clickSender.InteractionRespond(i, r, o...)
}

func (f *followSender) FollowupMessageCreate(
	_ *discordgo.Interaction, _ bool, p *discordgo.WebhookParams, _ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.follows = append(f.follows, p)
	return &discordgo.Message{}, nil
}

func (f *followSender) InteractionResponseDelete(*discordgo.Interaction, ...discordgo.RequestOption) error {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.deletions++
	return nil
}

func (f *followSender) ephemeralFollows(t *testing.T) []string {
	t.Helper()
	f.fmu.Lock()
	defer f.fmu.Unlock()
	var out []string
	for _, p := range f.follows {
		if p.Flags&discordgo.MessageFlagsEphemeral == 0 {
			t.Errorf("follow-up %q is not ephemeral", p.Content)
		}
		out = append(out, p.Content)
	}
	return out
}

func TestBot_WaitHandlersBoundedByDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := discord.NewWithSenderForTest(slog.New(slog.DiscardHandler), &followSender{}, "owner", "")
		started, release := make(chan struct{}), make(chan struct{})
		discord.SetHandlersForTest(b, map[string]card.Handler{
			"inc": func(context.Context, card.Action) (card.Result, error) {
				close(started)
				<-release
				return card.Result{}, nil
			},
		})
		go discord.OnInteractionForTest(b, dmClick("owner", "m1", btn("inc", "resolve", "1", "s").ID))
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.WaitHandlers(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("WaitHandlers with a stuck handler = %v, want DeadlineExceeded", err)
		}
		close(release)
		synctest.Wait()
		if err := b.WaitHandlers(context.Background()); err != nil {
			t.Errorf("WaitHandlers after release = %v", err)
		}
	})
}

func TestBot_PressWhileClosingGetsEphemeralNotice(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	var calls atomic.Int32
	e.handlers["inc"] = countingHandler(&calls, card.Result{Line: "ran"})
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	e.bot.StopIntake()
	e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
	rs := e.click.respondList()
	if len(rs) != 1 {
		t.Fatalf("responses = %d, want 1", len(rs))
	}
	if got := ephemeralContent(t, rs[0]); got != card.RestartingNotice {
		t.Errorf("content = %q, want the restarting notice", got)
	}
	if calls.Load() != 0 {
		t.Errorf("handler ran %d times while closing", calls.Load())
	}
}

func newFollowEnv(t *testing.T) (*clickEnv, *followSender) {
	t.Helper()
	d := openCardDB(t)
	api := &followSender{}
	b := discord.NewWithSenderForTest(slog.New(slog.DiscardHandler), api, "owner", "")
	t.Cleanup(func() { _ = b.Close() })
	return &clickEnv{
		cardEnv: &cardEnv{
			t: t, api: &api.editSender, bot: b, db: d, events: make(chan *db.Event, 8),
			handlers: map[string]card.Handler{},
		},
		click: &api.clickSender,
	}, api
}

func TestBot_HandlerRestartingErrorSendsEphemeralFollowup(t *testing.T) {
	t.Parallel()
	e, api := newFollowEnv(t)
	e.handlers["inc"] = func(context.Context, card.Action) (card.Result, error) {
		return card.Result{}, card.ErrRestarting
	}
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
	if got := api.ephemeralFollows(t); len(got) != 1 || got[0] != card.RestartingNotice {
		t.Fatalf("follow-ups = %q, want one restarting notice", got)
	}
	if discord.HasOverlayForTest(e.bot, "m9") {
		t.Error("a refused action left an overlay on the card")
	}
}

type quietNotifier struct{}

func (quietNotifier) NotifyOwner(context.Context, card.Card) error        { return nil }
func (quietNotifier) NotifyUser(context.Context, string, card.Card) error { return nil }

func reportInteraction(title string) *discordgo.InteractionCreate {
	opt := func(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
		return &discordgo.ApplicationCommandInteractionDataOption{
			Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v,
		}
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type: discordgo.InteractionApplicationCommand,
		Data: discordgo.ApplicationCommandInteractionData{
			Name: "report",
			Options: []*discordgo.ApplicationCommandInteractionDataOption{
				opt("what", "cant_play"), opt("title", title), opt("details", "it stops"),
			},
		},
		Member: &discordgo.Member{User: &discordgo.User{ID: "u1", Username: "alice"}},
	}}
}

func reportEnv(t *testing.T) (*discord.Bot, *followSender, *db.DB) {
	t.Helper()
	api := &followSender{}
	b, d := reportEnvWith(t, api)
	return b, api, d
}

func reportEnvWith(t *testing.T, api *followSender) (*discord.Bot, *db.DB) {
	t.Helper()
	d := openCardDB(t)
	b := discord.NewWithSenderForTest(slog.New(slog.DiscardHandler), api, "owner", "")
	t.Cleanup(func() { _ = b.Close() })
	svc := incident.NewService(
		context.Background(),
		d,
		nil,
		nil,
		nil,
		nil,
		quietNotifier{},
		slog.New(slog.DiscardHandler),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
		defer cancel()
		if err := svc.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	b.SetService(svc)
	return b, d
}

func TestBot_ReportWhileClosingIsStored(t *testing.T) {
	t.Parallel()
	b, api, d := reportEnv(t)
	b.StopIntake()
	discord.OnInteractionForTest(b, reportInteraction("Severance"))
	if _, err := d.FindOpenByTitle(context.Background(), "Severance"); err != nil {
		t.Fatalf("report during shutdown not stored: %v", err)
	}
	api.rmu.Lock()
	edits := append([]string(nil), api.followups...)
	api.rmu.Unlock()
	if len(edits) != 1 || !strings.Contains(edits[0], "Logged as **#") {
		t.Errorf("reply = %q, want the usual ack", edits)
	}
}

func TestBot_ReportStoreFailureWhileClosingGetsEphemeralNotice(t *testing.T) {
	t.Parallel()
	b, api, d := reportEnv(t)
	b.StopIntake()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	discord.OnInteractionForTest(b, reportInteraction("Severance"))
	if got := api.ephemeralFollows(t); len(got) != 1 || got[0] != card.RestartingNotice {
		t.Fatalf("follow-ups = %q, want one restarting notice", got)
	}
	api.fmu.Lock()
	defer api.fmu.Unlock()
	if api.deletions != 1 {
		t.Errorf("deferred public reply deletions = %d, want 1", api.deletions)
	}
}

func TestBot_DisconnectWaitsForReportStartedAfterWaitHandlers(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	api := &followSender{onDefer: func() {
		close(entered)
		<-release
	}}
	b, d := reportEnvWith(t, api)
	b.StopIntake()
	if err := b.WaitHandlers(context.Background()); err != nil {
		t.Fatalf("WaitHandlers with nothing running = %v", err)
	}
	reported := make(chan struct{})
	go func() {
		discord.OnInteractionForTest(b, reportInteraction("Severance"))
		close(reported)
	}()
	<-entered
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.Disconnect(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Disconnect with a /report still running = %v, want DeadlineExceeded", err)
	}
	disconnected := make(chan error, 1)
	go func() { disconnected <- b.Disconnect(context.Background()) }()
	select {
	case err := <-disconnected:
		t.Errorf("Disconnect returned %v before the /report finished", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-reported
	select {
	case err := <-disconnected:
		if err != nil {
			t.Errorf("Disconnect after the /report finished = %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("Disconnect did not return after the /report finished")
	}
	if _, err := d.FindOpenByTitle(context.Background(), "Severance"); err != nil {
		t.Errorf("report started before Disconnect not stored: %v", err)
	}
}

func TestBot_ReportAfterDisconnectGetsEphemeralNotice(t *testing.T) {
	t.Parallel()
	api := &followSender{}
	b, d := reportEnvWith(t, api)
	b.StopIntake()
	if err := b.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	discord.OnInteractionForTest(b, reportInteraction("Dark"))
	rs := api.respondList()
	if len(rs) != 1 {
		t.Fatalf("responses = %d, want 1", len(rs))
	}
	if got := ephemeralContent(t, rs[0]); got != card.RestartingNotice {
		t.Errorf("content = %q, want the restarting notice", got)
	}
	if _, err := d.FindOpenByTitle(context.Background(), "Dark"); err == nil {
		t.Error("report after Disconnect was stored")
	}
}

func TestBot_PressAfterDisconnectGetsEphemeralNotice(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	var calls atomic.Int32
	e.handlers["inc"] = countingHandler(&calls, card.Result{Line: "ran"})
	e.track("inc:1", "m9", incCard("1"), time.Now())
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	e.bot.StopIntake()
	if err := e.bot.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
	rs := e.click.respondList()
	if len(rs) != 1 {
		t.Fatalf("responses = %d, want 1", len(rs))
	}
	if got := ephemeralContent(t, rs[0]); got != card.RestartingNotice {
		t.Errorf("content = %q, want the restarting notice", got)
	}
	if calls.Load() != 0 {
		t.Errorf("handler ran %d times after Disconnect", calls.Load())
	}
}

func TestBot_DisconnectBoundsStuckSessionClose(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := discord.NewWithSenderForTest(slog.New(slog.DiscardHandler), &followSender{}, "owner", "")
		release := make(chan struct{})
		discord.SetSessionCloseForTest(b, func() error {
			<-release
			return nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.Disconnect(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Disconnect with a stuck session close = %v, want DeadlineExceeded", err)
		}
		close(release)
		synctest.Wait()
	})
}
