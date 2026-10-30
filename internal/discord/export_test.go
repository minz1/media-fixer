package discord

import (
	"context"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
)

func NewForTest(log *slog.Logger, connect func() error, retryFirst, retryCap time.Duration) (*Bot, error) {
	b, err := New("x", "", "", "", log)
	if err != nil {
		return nil, err
	}
	b.connect, b.retryFirst, b.retryCap = connect, retryFirst, retryCap
	return b, nil
}

func ReportCommandForTest() *discordgo.ApplicationCommand { return reportCommand() }

func PlainTextForTest(c card.Card) string { return plainText(c) }

func NewWithSenderForTest(log *slog.Logger, api sender, ownerID, publicURL string) *Bot {
	return &Bot{api: api, ownerID: ownerID, publicURL: publicURL, log: log}
}

func RenderForTest(c card.Card, publicURL string) []discordgo.MessageComponent {
	return render(c, nil, overlay{}, publicURL)
}

func SendForTest(ctx context.Context, b *Bot, userID string, c card.Card) (*discordgo.Message, bool, error) {
	return b.send(ctx, userID, c)
}
