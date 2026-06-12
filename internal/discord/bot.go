package discord

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/minz1/mediafixer/internal/incident"
)

// Bot wraps discordgo and registers the /report slash command.
type Bot struct {
	session *discordgo.Session
	guildID string
	ownerID string
	svc     *incident.Service
	log     *slog.Logger
}

func New(token, guildID, ownerID string, log *slog.Logger) (*Bot, error) {
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discordgo: %w", err)
	}
	b := &Bot{
		session: sess,
		guildID: guildID,
		ownerID: ownerID,
		log:     log,
	}
	sess.AddHandler(b.onInteraction)
	return b, nil
}

// SetService wires the incident service after construction (breaks the
// mutual dependency cycle between Bot and Service).
func (b *Bot) SetService(svc *incident.Service) {
	b.svc = svc
}

var reportCommand = &discordgo.ApplicationCommand{
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
			Description: "Any extra details (optional)",
			Required:    false,
		},
	},
}

func (b *Bot) Start() error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("discord open: %w", err)
	}

	_, err := b.session.ApplicationCommandCreate(b.session.State.User.ID, b.guildID, reportCommand)
	if err != nil {
		return fmt.Errorf("register /report command: %w", err)
	}

	b.log.Info("discord bot started")
	return nil
}

func (b *Bot) Close() error {
	return b.session.Close()
}

// NotifyOwner sends a DM to the configured owner user ID.
func (b *Bot) NotifyOwner(ctx context.Context, msg string) error {
	ch, err := b.session.UserChannelCreate(b.ownerID)
	if err != nil {
		return fmt.Errorf("create DM channel: %w", err)
	}
	_, err = b.session.ChannelMessageSend(ch.ID, msg)
	return err
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
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
		Source:     "discord",
		ReportedBy: reporter,
		What:       what,
		Title:      title,
		Details:    details,
	})

	var content string
	if err != nil {
		b.log.Error("handle report", "error", err)
		content = "❌ Failed to create incident. Please try again."
	} else {
		content = fmt.Sprintf("✅ Incident **#%s** created for **%s**. The agent is investigating — you'll get a DM when it's done.",
			inc.ID[:8], title)
	}

	_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: &content,
	})
}

func optionMap(opts []*discordgo.ApplicationCommandInteractionDataOption) map[string]*discordgo.ApplicationCommandInteractionDataOption {
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
