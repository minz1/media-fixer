package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

const (
	livePoll     = 30 * time.Second
	cardKeep     = 14 * 24 * time.Hour
	pruneEvery   = 24 * time.Hour
	prefixSuffix = ":"
)

type cardState struct {
	pending  map[string]bool
	messages map[string]bool
	overlays map[string]overlay
	store    *db.DB
}

// Refresh queues a re-render of every message tracking subjectOrPrefix (a prefix ends in ":").
func (b *Bot) Refresh(subjectOrPrefix string) {
	b.mu.Lock()
	b.cards.pending[subjectOrPrefix] = true
	b.mu.Unlock()
	b.wakeCards()
}

func (b *Bot) setOverlay(messageID string, ov overlay) {
	b.mu.Lock()
	b.cards.overlays[messageID] = ov
	b.cards.messages[messageID] = true
	b.mu.Unlock()
	b.wakeCards()
}

func (b *Bot) wakeCards() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// StartCards starts the updater that keeps tracked owner cards current.
func (b *Bot) StartCards(
	ctx context.Context, d *db.DB, events <-chan *db.Event, renderers map[string]card.Renderer,
) {
	b.mu.Lock()
	b.cards.store = d
	b.mu.Unlock()
	b.renderers = renderers
	ctx, b.stopCards = context.WithCancel(ctx)
	go b.cardLoop(ctx, d, events)
}

func (b *Bot) cardStore() *db.DB {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cards.store
}

func (b *Bot) cardLoop(ctx context.Context, d *db.DB, events <-chan *db.Event) {
	defer close(b.cardsDone)
	b.prune(ctx, d)
	if rows, err := d.ListDiscordCards(ctx, ""); err != nil {
		b.log.WarnContext(ctx, "discord cards: startup refresh", "error", err)
	} else {
		b.refreshMessages(ctx, d, messageIDs(rows))
	}
	poll := time.NewTicker(b.poll)
	defer poll.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	var last int64
	for {
		var extra []string
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			last = b.onEvent(ev, last)
		case <-b.wake:
		case <-poll.C:
			extra = slices.Collect(maps.Keys(b.liveMsgs))
		case <-prune.C:
			b.prune(ctx, d)
		}
		b.refreshMessages(ctx, d, append(b.takePending(ctx, d), extra...))
	}
}

func (b *Bot) onEvent(ev *db.Event, last int64) int64 {
	if last > 0 && ev.Seq > last+1 {
		b.Refresh(card.IncidentSubject(""))
	}
	if slices.Contains(
		[]journal.Kind{journal.KindStatusChanged, journal.KindDiagnosisCompleted, journal.KindRunFinished},
		journal.Kind(ev.Kind),
	) {
		b.Refresh(card.IncidentSubject(ev.IncidentID))
	}
	return max(last, ev.Seq)
}

func (b *Bot) takePending(ctx context.Context, d *db.DB) []string {
	b.mu.Lock()
	subjects, ids := b.cards.pending, slices.Collect(maps.Keys(b.cards.messages))
	b.cards.pending, b.cards.messages = map[string]bool{}, map[string]bool{}
	b.mu.Unlock()
	for s := range subjects {
		var (
			rows []db.DiscordCard
			err  error
		)
		if strings.HasSuffix(s, prefixSuffix) {
			rows, err = d.ListDiscordCards(ctx, s)
		} else {
			rows, err = d.ListDiscordSubject(ctx, s)
		}
		if err != nil {
			b.log.WarnContext(ctx, "discord cards: list", "subject", s, "error", err)
			continue
		}
		ids = append(ids, messageIDs(rows)...)
	}
	return ids
}

func messageIDs(rows []db.DiscordCard) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.MessageID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (b *Bot) refreshMessages(ctx context.Context, d *db.DB, ids []string) {
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		b.refreshMessage(ctx, d, id)
	}
}

func (b *Bot) refreshMessage(ctx context.Context, d *db.DB, id string) {
	rows, err := d.ListDiscordMessage(ctx, id)
	if err != nil {
		b.log.WarnContext(ctx, "discord cards: load message", "message", id, "error", err)
		return
	}
	if len(rows) == 0 {
		b.forget(id)
		return
	}
	var c card.Card
	if err = json.Unmarshal(rows[0].Card, &c); err != nil {
		b.log.WarnContext(ctx, "discord cards: decode card", "message", id, "error", err)
		return
	}
	ups := make(map[string]card.Update, len(rows))
	live := false
	for _, r := range rows {
		up, ok := b.renderSubject(ctx, r.Subject)
		if !ok {
			return
		}
		ups[r.Subject] = keepSentVerbs(c, r.Subject, up)
		live = live || up.Live
	}
	if live {
		b.liveMsgs[id] = true
	} else {
		delete(b.liveMsgs, id)
	}
	ov := b.pinOverlay(id, ups)
	comps := render(c, ups, ov, b.publicURL)
	hash, err := renderHash(comps)
	if err != nil || hash == rows[0].Hash {
		return
	}
	_, err = b.api.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID: id, Channel: rows[0].ChannelID, Components: &comps, Flags: discordgo.MessageFlagsIsComponentsV2,
	})
	var rest *discordgo.RESTError
	switch {
	case errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound:
		if err = d.DeleteDiscordMessage(ctx, id); err != nil {
			b.log.WarnContext(ctx, "discord cards: drop deleted message", "message", id, "error", err)
		}
		b.forget(id)
	case err != nil:
		b.log.WarnContext(ctx, "discord card edit failed", "message", id, "error", err)
	default:
		if err = d.SetDiscordMessageHash(ctx, id, hash); err != nil {
			b.log.WarnContext(ctx, "discord cards: store hash", "message", id, "error", err)
		}
	}
}

func (b *Bot) renderSubject(ctx context.Context, subject string) (card.Update, bool) {
	r, ok := b.renderers[card.Kind(subject)]
	if !ok {
		return card.Update{}, false
	}
	up, ok, err := r(ctx, subject)
	if err != nil {
		b.log.WarnContext(ctx, "discord cards: render", "subject", subject, "error", err)
		return card.Update{}, false
	}
	return up, ok
}

func keepSentVerbs(c card.Card, subject string, up card.Update) card.Update {
	sent := c.Verbs
	if subject != c.Subject {
		sent = nil
		for _, it := range c.Items {
			if it.Subject == subject {
				sent = buttonVerbs(it.Buttons)
			}
		}
	}
	up.Buttons = slices.DeleteFunc(slices.Clone(up.Buttons), func(bt card.Button) bool {
		a, err := card.Decode(bt.ID)
		return err != nil || !slices.Contains(sent, a.Verb)
	})
	return up
}

func buttonVerbs(bs []card.Button) []string {
	var out []string
	for _, bt := range bs {
		if a, err := card.Decode(bt.ID); err == nil {
			out = append(out, a.Verb)
		}
	}
	return out
}

func (b *Bot) pinOverlay(id string, ups map[string]card.Update) overlay {
	h, err := json.Marshal(ups)
	if err != nil {
		return overlay{}
	}
	updateHash := card.Fingerprint(h)
	b.mu.Lock()
	defer b.mu.Unlock()
	ov, ok := b.cards.overlays[id]
	switch {
	case !ok:
		return overlay{}
	case ov.UpdateHash == "":
		ov.UpdateHash = updateHash
		b.cards.overlays[id] = ov
	case ov.UpdateHash != updateHash:
		delete(b.cards.overlays, id)
		return overlay{}
	}
	return ov
}

func (b *Bot) forget(id string) {
	delete(b.liveMsgs, id)
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cards.overlays, id)
}

func (b *Bot) prune(ctx context.Context, d *db.DB) {
	ids, err := d.PruneDiscordCards(ctx, time.Now().Add(-cardKeep))
	if err != nil {
		b.log.WarnContext(ctx, "discord cards: prune", "error", err)
		return
	}
	for _, id := range ids {
		b.forget(id)
	}
}

func renderHash(comps []discordgo.MessageComponent) (string, error) {
	j, err := json.Marshal(comps)
	if err != nil {
		return "", fmt.Errorf("hash render: %w", err)
	}
	return card.Fingerprint(j), nil
}

func cardSubjects(c card.Card) []string {
	var out []string
	if c.Subject != "" {
		out = append(out, c.Subject)
	}
	for _, it := range c.Items {
		if it.Subject != "" {
			out = append(out, it.Subject)
		}
	}
	return out
}

func (b *Bot) record(ctx context.Context, d *db.DB, m *discordgo.Message, c card.Card, subjects []string) error {
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode card: %w", err)
	}
	hash, err := renderHash(render(c, nil, overlay{}, b.publicURL))
	if err != nil {
		return err
	}
	now := time.Now()
	rows := make([]db.DiscordCard, 0, len(subjects))
	for _, s := range subjects {
		rows = append(rows, db.DiscordCard{
			Subject: s, ChannelID: m.ChannelID, MessageID: m.ID, Card: body, Hash: hash, SentAt: now,
		})
	}
	if err = d.InsertDiscordCards(ctx, rows); err != nil {
		return fmt.Errorf("record card: %w", err)
	}
	for _, s := range subjects {
		b.Refresh(s)
	}
	return nil
}
