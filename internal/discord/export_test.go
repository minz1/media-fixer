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

func SetCardRetryForTest(b *Bot, first, ceiling time.Duration) {
	b.cardRetryFirst, b.cardRetryCap = first, ceiling
}

func SetPassDoneForTest(b *Bot, fn func()) { b.onPass = fn }

func SetTimerFiredForTest(b *Bot, fn func()) { b.onTimer = fn }

func GatewayForTest(b *Bot, up bool) {
	if up {
		b.onConnect(nil, &discordgo.Connect{})
		return
	}
	b.onDisconnect(nil, &discordgo.Disconnect{})
}

func SetResyncEveryForTest(b *Bot, d time.Duration) { b.resyncEvery = d }

func SetConfirmForTest(b *Bot, d time.Duration) { b.confirmFor = d }

func CardsDoneForTest(b *Bot) <-chan struct{} { return b.cardsDone }

func OnInteractionForTest(b *Bot, i *discordgo.InteractionCreate) { b.onInteraction(nil, i) }

func SetClockForTest(b *Bot, now func() time.Time) { b.now = now }

func RenderUpdatesForTest(c card.Card, u map[string]card.Update) []discordgo.MessageComponent {
	return render(c, u, overlay{}, "")
}

func RenderOverlayForTest(
	c card.Card, u map[string]card.Update, line string, confirm []card.Button, publicURL string,
) []discordgo.MessageComponent {
	return render(c, u, overlay{Line: line, Confirm: confirm}, publicURL)
}

func TextLenForTest(cs []discordgo.MessageComponent) int { return textLen(cs) }

func SetConnectForTest(b *Bot, connect func() error) { b.connect = connect }

func SetHandlersForTest(b *Bot, handlers map[string]card.Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers, b.procCtx = handlers, context.Background()
}

func SetSessionCloseForTest(b *Bot, fn func() error) { b.closeConn = fn }

type (
	CardPhase     = cardPhase
	CardKind      = cardKind
	CardSnap      = cardSnap
	CardEvent     = cardEvent
	CardRetry     = cardRetry
	ConnKind      = connKind
	ConnEvent     = connEvent
	RejectedError = rejectedError
)

const (
	CardCurrent     = cardCurrent
	CardDirty       = cardDirty
	CardLive        = cardLive
	CardFailing     = cardFailing
	CardGone        = cardGone
	CardMark        = cardMark
	CardPress       = cardPress
	CardRetryDue    = cardRetryDue
	CardShown       = cardShown
	CardHeld        = cardHeld
	CardMissing     = cardMissing
	CardUndecodable = cardUndecodable
	CardAllGone     = cardAllGone
	CardFailed      = cardFailed
	CardPruned      = cardPruned

	ConnStart       = connStart
	ConnOpenOK      = connOpenOK
	ConnOpenFailed  = connOpenFailed
	ConnRetryDue    = connRetryDue
	ConnGatewayDown = connGatewayDown
	ConnGatewayUp   = connGatewayUp
	ConnStop        = connStop
)

func CardPhases() []CardPhase { return cardPhases() }

func CardKinds() []CardKind { return cardKinds() }

func CardMatches(p CardPhase, k CardKind) int { return cardMatches(p, k) }

func NextCard(s CardSnap, ev CardEvent) (CardSnap, error) { return nextCard(s, ev) }

func CheckCard(s CardSnap) error { return checkCard(s) }

func CardBackoff(n int, r CardRetry) time.Duration { return cardBackoff(n, r) }

func ConnStates() []ConnState { return connStates() }

func ConnKinds() []ConnKind { return connKinds() }

func ConnMatches(s ConnState, k ConnKind) int { return connMatches(s, k) }

func NextConn(c Conn, ev ConnEvent) (Conn, error) { return nextConn(c, ev) }

func CheckConn(c Conn) error { return checkConn(c) }
