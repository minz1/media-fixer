package discord

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
)

type sender interface {
	UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageSendComplex(
		channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption,
	) (*discordgo.Message, error)
	ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
	InteractionRespond(
		interaction *discordgo.Interaction, resp *discordgo.InteractionResponse, options ...discordgo.RequestOption,
	) error
	InteractionResponseEdit(
		interaction *discordgo.Interaction, newresp *discordgo.WebhookEdit, options ...discordgo.RequestOption,
	) (*discordgo.Message, error)
}

func (b *Bot) send(ctx context.Context, userID string, c card.Card) (*discordgo.Message, bool, error) {
	ch, err := b.api.UserChannelCreate(userID)
	if err != nil {
		return nil, false, fmt.Errorf("create DM channel: %w", err)
	}
	m, err := b.api.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
		Components: render(c, nil, overlay{}, b.publicURL),
		Flags:      discordgo.MessageFlagsIsComponentsV2,
	})
	if err == nil {
		b.log.InfoContext(ctx, "discord card sent", "plain", false)
		return m, false, nil
	}
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) || rest.Response == nil ||
		rest.Response.StatusCode < http.StatusBadRequest || rest.Response.StatusCode >= http.StatusInternalServerError {
		return nil, false, fmt.Errorf("send card: %w", err)
	}
	m, err = b.api.ChannelMessageSend(ch.ID, plainText(c))
	if err != nil {
		return nil, true, fmt.Errorf("send plain after %s: %w", rest.Error(), err)
	}
	b.log.InfoContext(ctx, "discord card sent", "plain", true, "status", rest.Response.StatusCode, "error", rest)
	return m, true, nil
}
