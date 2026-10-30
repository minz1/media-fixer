package discord

import (
	"context"
	"slices"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
)

const (
	handlerTimeout   = 10 * time.Minute
	confirmWindow    = 2 * time.Minute
	verbConfirm      = "confirm"
	msgNotOwner      = "Only the owner can use these buttons"
	msgOldButton     = "This button is from an older version; use the dashboard link."
	msgRunning       = "That action is already running."
	msgConfirmGone   = "Refused: the confirmation expired; press Approve again"
	msgConfirmLost   = "Refused: this card is no longer tracked; use the dashboard link."
	msgDone          = "Done."
	failedLinePrefix = "⚠️ Failed: "
)

func (b *Bot) onComponent(i *discordgo.InteractionCreate) {
	ctx := context.Background()
	clicker := i.User
	if i.Member != nil {
		clicker = i.Member.User
	}
	if clicker == nil || b.ownerID == "" || clicker.ID != b.ownerID {
		userID := ""
		if clicker != nil {
			userID = clicker.ID
		}
		b.log.WarnContext(ctx, "discord button pressed by someone other than the owner", "user", userID)
		b.ephemeral(ctx, i, msgNotOwner)
		return
	}
	customID := i.MessageComponentData().CustomID
	a, err := card.Decode(customID)
	procCtx, h, ok := b.handler(a.Kind)
	if err != nil || !ok {
		b.ephemeral(ctx, i, msgOldButton)
		return
	}
	subject := card.SubjectOf(a)
	if !b.begin(subject) {
		b.ephemeral(ctx, i, msgRunning)
		return
	}
	defer b.end(subject)
	msgID := i.Message.ID
	tracked := b.tracked(ctx, msgID)
	ack := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}
	if !tracked {
		ack = &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
		}
	}
	if err = b.api.InteractionRespond(i.Interaction, ack); err != nil {
		b.log.WarnContext(ctx, "discord button ack failed", "button", customID, "error", err)
		return
	}
	res := b.runAction(procCtx, msgID, customID, h, a)
	if tracked {
		b.setOverlay(msgID, overlay{Line: res.Line, Confirm: res.Confirm, At: b.now()})
		if subject != "" {
			b.Refresh(subject)
		}
		return
	}
	line := res.Line
	if len(res.Confirm) > 0 {
		line = msgConfirmLost
	}
	if line == "" {
		line = msgDone
	}
	if _, err = b.api.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &line}); err != nil {
		b.log.WarnContext(ctx, "discord button reply failed", "button", customID, "error", err)
	}
}

func (b *Bot) runAction(
	procCtx context.Context, msgID, customID string, h card.Handler, a card.Action,
) card.Result {
	if a.Verb == verbConfirm && !b.confirmOpen(msgID, customID) {
		return card.Result{Line: msgConfirmGone}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(procCtx), handlerTimeout)
	defer cancel()
	res, err := h(ctx, a)
	if err != nil {
		return card.Result{Line: failedLinePrefix + err.Error()}
	}
	return res
}

func (b *Bot) handler(kind string) (context.Context, card.Handler, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h, ok := b.handlers[kind]
	return b.procCtx, h, ok
}

func (b *Bot) begin(subject string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.inFlight[subject] {
		return false
	}
	b.inFlight[subject] = true
	b.running.Add(1)
	return true
}

func (b *Bot) end(subject string) {
	b.mu.Lock()
	delete(b.inFlight, subject)
	b.mu.Unlock()
	b.running.Done()
}

func (b *Bot) tracked(ctx context.Context, msgID string) bool {
	d := b.cardStore()
	if d == nil {
		return false
	}
	rows, err := d.ListDiscordMessage(ctx, msgID)
	if err != nil {
		b.log.WarnContext(ctx, "discord cards: load clicked message", "message", msgID, "error", err)
		return false
	}
	return len(rows) > 0
}

func (b *Bot) confirmOpen(msgID, customID string) bool {
	b.mu.Lock()
	ov, ok := b.cards.overlays[msgID]
	b.mu.Unlock()
	return ok && b.now().Sub(ov.At) < confirmWindow &&
		slices.ContainsFunc(ov.Confirm, func(bt card.Button) bool { return bt.ID == customID })
}

func (b *Bot) ephemeral(ctx context.Context, i *discordgo.InteractionCreate, msg string) {
	if err := b.api.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg, Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		b.log.WarnContext(ctx, "discord button reply failed", "error", err)
	}
}
