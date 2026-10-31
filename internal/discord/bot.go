package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/incident"
)

const (
	connectRetryFirst = 5 * time.Second
	connectRetryCap   = 5 * time.Minute
)

// Bot wraps discordgo and registers the /report slash command.
type Bot struct {
	session   *discordgo.Session
	api       sender
	guildID   string
	ownerID   string
	publicURL string
	svc       *incident.Service
	log       *slog.Logger

	connect    func() error
	retryFirst time.Duration
	retryCap   time.Duration
	cancel     context.CancelFunc

	mu        sync.Mutex
	cards     cardState
	wake      chan struct{}
	poll      time.Duration
	renderers map[string]card.Renderer
	handlers  map[string]card.Handler
	procCtx   context.Context
	inFlight  map[string]bool
	closed    bool
	running   sync.WaitGroup
	now       func() time.Time
	liveMsgs  map[string]bool
	stopCards context.CancelFunc
	cardsDone chan struct{}
}

// New creates a Bot from a Discord bot token.
func New(token, guildID, ownerID, publicURL string, log *slog.Logger) (*Bot, error) {
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discordgo: %w", err)
	}
	b := &Bot{
		session:    sess,
		api:        sess,
		guildID:    guildID,
		ownerID:    ownerID,
		publicURL:  publicURL,
		log:        log,
		retryFirst: connectRetryFirst,
		retryCap:   connectRetryCap,
		cards: cardState{
			pending: map[string]bool{}, messages: map[string]bool{}, overlays: map[string]overlay{},
		},
		wake:      make(chan struct{}, 1),
		poll:      livePoll,
		liveMsgs:  map[string]bool{},
		inFlight:  map[string]bool{},
		now:       time.Now,
		cardsDone: make(chan struct{}),
	}
	b.connect = b.openAndRegister
	sess.AddHandler(b.onInteraction)
	return b, nil
}

// SetService wires the incident service after construction (breaks the
// mutual dependency cycle between Bot and Service).
func (b *Bot) SetService(svc *incident.Service) {
	b.svc = svc
}

// Start connects to the Discord gateway and registers the /report command in
// the background, retrying with backoff until it succeeds or ctx ends.
func (b *Bot) Start(ctx context.Context) {
	ctx, b.cancel = context.WithCancel(ctx)
	go b.connectLoop(ctx)
}

func (b *Bot) connectLoop(ctx context.Context) {
	wait := b.retryFirst
	for attempt := 1; ; attempt++ {
		err := b.connect()
		if err == nil {
			if ctx.Err() != nil {
				_ = b.session.Close()
				return
			}
			b.log.InfoContext(ctx, "discord bot started")
			return
		}
		b.log.ErrorContext(ctx, "discord connect failed", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait+wait, b.retryCap)
	}
}

func reportCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        "report",
		Description: "Report a media playback problem",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "what",
				Description: "What is the problem?",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "Can't play", Value: "cant_play"},
					{Name: "It plays but buffers / low quality", Value: "buffering"},
					{Name: "Login failed", Value: "login_failed"},
					{Name: "Missing media", Value: "missing_media"},
					{Name: "Other", Value: "other"},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "title",
				Description: "Show or movie title",
				Required:    true,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "details",
				Description: "Describe what you're seeing",
				Required:    true,
			},
		},
	}
}

func (b *Bot) openAndRegister() error {
	cmd := reportCommand()
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}

	_, err := b.session.ApplicationCommandCreate(b.session.State.User.ID, b.guildID, cmd)
	if err != nil {
		_ = b.session.Close()
		return fmt.Errorf("register /report command: %w", err)
	}
	return nil
}

// Close stops any pending connect retry, closes the Discord gateway connection, waits for running button
// handlers, then stops the card updater.
func (b *Bot) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	err := b.session.Close()
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.running.Wait()
	if b.stopCards != nil {
		b.stopCards()
		<-b.cardsDone
	}
	return err
}

// NotifyOwner sends a DM to the configured owner user ID and tracks a card with subjects for live updates.
func (b *Bot) NotifyOwner(ctx context.Context, c card.Card) error {
	m, plain, err := b.send(ctx, b.ownerID, c)
	if err != nil {
		return err
	}
	subjects := cardSubjects(c)
	if len(subjects) == 0 {
		return nil
	}
	d := b.cardStore()
	if plain || d == nil {
		return card.ErrUntracked
	}
	if err = b.record(ctx, d, m, c, subjects); err != nil {
		b.log.ErrorContext(ctx, "discord card sent but not recorded", "err", err)
		return card.ErrUntracked
	}
	return nil
}

// NotifyUser sends a DM to an arbitrary Discord user ID.
func (b *Bot) NotifyUser(ctx context.Context, userID string, c card.Card) error {
	_, _, err := b.send(ctx, userID, c)
	return err
}

func plainText(c card.Card) string {
	s := c.Plain()
	if len(s) <= card.MaxPlain {
		return s
	}
	cut := card.MaxPlain
	for !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type == discordgo.InteractionMessageComponent {
		b.onComponent(i)
		return
	}
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if i.ApplicationCommandData().Name != "report" {
		return
	}

	opts := optionMap(i.ApplicationCommandData().Options)
	what := optStr(opts, "what")
	title := optStr(opts, "title")
	details := optStr(opts, "details")

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	reporter := i.Member.User.Username
	if i.Member.Nick != "" {
		reporter = i.Member.Nick
	}

	inc, err := b.svc.Handle(context.Background(), &incident.Report{
		Source:            "discord",
		ReportedBy:        reporter,
		ReporterDiscordID: i.Member.User.ID,
		What:              what,
		Title:             title,
		Details:           details,
	})

	var content string
	if err != nil {
		b.log.Error("handle report", "error", err)
		content = "❌ Failed to create incident. Please try again."
	} else {
		content = fmt.Sprintf(
			"✅ Incident **#%s** created for **%s**. The agent is investigating. You'll get a DM when it's resolved.",
			inc.ID[:8],
			title,
		)
	}

	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content,
	})
}

func optionMap(
	opts []*discordgo.ApplicationCommandInteractionDataOption,
) map[string]*discordgo.ApplicationCommandInteractionDataOption {
	m := make(map[string]*discordgo.ApplicationCommandInteractionDataOption, len(opts))
	for _, o := range opts {
		m[o.Name] = o
	}
	return m
}

func optStr(m map[string]*discordgo.ApplicationCommandInteractionDataOption, key string) string {
	if o, ok := m[key]; ok {
		return o.StringValue()
	}
	return ""
}
