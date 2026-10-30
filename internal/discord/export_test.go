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
	b, err := New("x", "", ownerID, publicURL, log)
	if err != nil {
		panic(err)
	}
	b.api = api
	return b
}

func RenderForTest(c card.Card, publicURL string) []discordgo.MessageComponent {
	return render(c, nil, overlay{}, publicURL)
}

func SendForTest(ctx context.Context, b *Bot, userID string, c card.Card) (*discordgo.Message, bool, error) {
	return b.send(ctx, userID, c)
}

func SetOverlayForTest(b *Bot, messageID, line string) { b.setOverlay(messageID, overlay{Line: line}) }

func HasOverlayForTest(b *Bot, messageID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.cards.overlays[messageID]
	return ok
}

func SetPollIntervalForTest(b *Bot, d time.Duration) { b.poll = d }

func CardsDoneForTest(b *Bot) <-chan struct{} { return b.cardsDone }

func OnInteractionForTest(b *Bot, i *discordgo.InteractionCreate) { b.onInteraction(nil, i) }

func SetClockForTest(b *Bot, now func() time.Time) { b.now = now }

func RenderUpdatesForTest(c card.Card, u map[string]card.Update) []discordgo.MessageComponent {
	return render(c, u, overlay{}, "")
}
