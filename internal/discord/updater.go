package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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

	detailsTooLong = "Details too long for Discord; review them on the dashboard"
)

type cardState struct {
	pending  map[string]bool
	pressed  map[string]bool
	overlays map[string]overlay
	store    *db.DB
	warned   map[string]bool
}

// Refresh queues a re-render of every message tracking subjectOrPrefix (a prefix ends in ":").
func (b *Bot) Refresh(subjectOrPrefix string) {
	b.mu.Lock()
	b.cards.pending[subjectOrPrefix] = true
	b.mu.Unlock()
	b.wakeCards()
}

func (b *Bot) setOverlay(messageID string, ov overlay) {
	if len(ov.Confirm) > 0 {
		ov.ExpiresAt = b.now().Add(b.confirmFor)
	}
	b.mu.Lock()
	if ov.Line == "" && len(ov.Confirm) == 0 {
		delete(b.cards.overlays, messageID)
	} else {
		b.cards.overlays[messageID] = ov
	}
	b.cards.pressed[messageID] = true
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
	ctx context.Context, d *db.DB, events <-chan *db.Event,
	handlers map[string]card.Handler, renderers map[string]card.Renderer,
) {
	b.mu.Lock()
	b.cards.store = d
	b.handlers, b.procCtx = handlers, ctx
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
	msgs := map[string]cardSnap{}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	b.cardPass(ctx, d, msgs, b.startupTriggers(ctx, d), timer)
	poll := time.NewTicker(b.poll)
	defer poll.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	var last int64
	for {
		trig := map[string]cardKind{}
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			last = b.onEvent(ev, last)
		case <-b.wake:
		case <-timer.C:
			if b.onTimer != nil {
				b.onTimer()
			}
		case <-poll.C:
			for id, s := range msgs {
				if s.Phase == cardLive {
					addTrigger(trig, id, cardPoll)
				}
			}
		case <-prune.C:
			for _, id := range b.prune(ctx, d) {
				delete(msgs, id)
			}
		}
		b.collect(ctx, d, trig)
		b.cardPass(ctx, d, msgs, trig, timer)
	}
}

func (b *Bot) startupTriggers(ctx context.Context, d *db.DB) map[string]cardKind {
	b.prune(ctx, d)
	trig := map[string]cardKind{}
	b.collect(ctx, d, trig)
	rows, err := d.ListDiscordCards(ctx, "")
	if err != nil {
		b.log.WarnContext(ctx, "discord cards: startup refresh", "error", err)
		return trig
	}
	for _, id := range messageIDs(rows) {
		addTrigger(trig, id, cardMark)
	}
	return trig
}

func addTrigger(trig map[string]cardKind, id string, k cardKind) {
	rank := func(k cardKind) int { return slices.Index([]cardKind{cardPress, cardRetryDue, cardMark, cardPoll}, k) }
	if cur, ok := trig[id]; !ok || rank(k) < rank(cur) {
		trig[id] = k
	}
}

func (b *Bot) collect(ctx context.Context, d *db.DB, trig map[string]cardKind) {
	marks, pressed := b.takePending(ctx, d)
	for _, id := range marks {
		addTrigger(trig, id, cardMark)
	}
	for _, id := range pressed {
		addTrigger(trig, id, cardPress)
	}
}

func (b *Bot) cardPass(
	ctx context.Context, d *db.DB, msgs map[string]cardSnap, trig map[string]cardKind, timer *time.Timer,
) {
	now := b.now()
	for id, s := range msgs {
		if s.Phase == cardFailing && !s.NextRetry.After(now) {
			addTrigger(trig, id, cardRetryDue)
		}
	}
	for _, id := range b.expireConfirms(now) {
		addTrigger(trig, id, cardMark)
	}
	for _, id := range slices.Sorted(maps.Keys(trig)) {
		if ctx.Err() != nil {
			break
		}
		b.cardStep(ctx, d, msgs, id, trig[id])
	}
	b.armTimer(msgs, timer)
	if b.onPass != nil {
		b.onPass()
	}
}

func (b *Bot) armTimer(msgs map[string]cardSnap, timer *time.Timer) {
	timer.Stop()
	next := b.nextConfirmExpiry()
	for _, s := range msgs {
		if s.Phase == cardFailing && (next.IsZero() || s.NextRetry.Before(next)) {
			next = s.NextRetry
		}
	}
	if !next.IsZero() {
		timer.Reset(max(next.Sub(b.now()), 0))
	}
}

func (b *Bot) expireConfirms(now time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for id, ov := range b.cards.overlays {
		if b.dropExpiredLocked(id, ov, now) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (b *Bot) dropExpiredLocked(id string, ov overlay, now time.Time) bool {
	if len(ov.Confirm) == 0 || now.Before(ov.ExpiresAt) {
		return false
	}
	ov.Confirm, ov.ExpiresAt = nil, time.Time{}
	if ov.Line == "" {
		delete(b.cards.overlays, id)
	} else {
		b.cards.overlays[id] = ov
	}
	return true
}

func (b *Bot) nextConfirmExpiry() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	var next time.Time
	for _, ov := range b.cards.overlays {
		if len(ov.Confirm) > 0 && (next.IsZero() || ov.ExpiresAt.Before(next)) {
			next = ov.ExpiresAt
		}
	}
	return next
}

func (b *Bot) cardEventAt(kind cardKind) cardEvent {
	return cardEvent{Kind: kind, At: b.now(), Retry: cardRetry{First: b.cardRetryFirst, Cap: b.cardRetryCap}}
}

func (b *Bot) applyCard(ctx context.Context, id string, s cardSnap, ev cardEvent) cardSnap {
	next, err := nextCard(s, ev)
	if err != nil {
		b.log.DebugContext(ctx, "discord card event rejected", "message", id, "error", err)
		return s
	}
	return next
}

func (b *Bot) cardStep(ctx context.Context, d *db.DB, msgs map[string]cardSnap, id string, kind cardKind) {
	snap, ok := msgs[id]
	if !ok {
		snap = cardSnap{Phase: cardCurrent}
	}
	snap = b.applyCard(ctx, id, snap, b.cardEventAt(kind))
	if snap.Phase != cardDirty {
		msgs[id] = snap
		return
	}
	res := b.refreshMessage(ctx, d, id)
	if res.Kind == cardFailed && ctx.Err() != nil {
		return
	}
	ev := b.cardEventAt(res.Kind)
	ev.Live = res.Live
	var limited *discordgo.RateLimitError
	if errors.As(res.Err, &limited) && limited.RateLimit != nil && limited.TooManyRequests != nil {
		ev.RetryAfter = limited.RetryAfter
	}
	next := b.applyCard(ctx, id, snap, ev)
	b.cardEffects(ctx, d, id, snap, next, res)
	if next.Phase == cardGone {
		delete(msgs, id)
		b.forget(id)
		return
	}
	msgs[id] = next
}

func (b *Bot) cardEffects(
	ctx context.Context, d *db.DB, id string, before, after cardSnap, res attemptResult,
) {
	switch {
	case res.Kind == cardUndecodable && after.Phase == cardGone:
		if err := d.DeleteDiscordMessage(ctx, id); err != nil {
			b.log.WarnContext(ctx, "discord cards: drop undecodable message", "message", id, "error", err)
		}
		b.log.WarnContext(ctx, "discord card undecodable, dropped", "message", id, "error", res.Err)
	case after.Phase == cardFailing && after.Failures == 1:
		b.log.WarnContext(ctx, "discord card edit failed", "message", id, "error", res.Err)
	case after.Phase == cardFailing:
		b.log.InfoContext(ctx, "discord card edit failed", "message", id, "failures", after.Failures,
			"error", res.Err)
	case before.Failures > 0 && (after.Phase == cardCurrent || after.Phase == cardLive):
		b.log.InfoContext(ctx, "discord card edit recovered", "message", id, "failures", before.Failures)
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

func (b *Bot) takePending(ctx context.Context, d *db.DB) ([]string, []string) {
	b.mu.Lock()
	subjects, pressed := b.cards.pending, slices.Collect(maps.Keys(b.cards.pressed))
	b.cards.pending, b.cards.pressed = map[string]bool{}, map[string]bool{}
	b.mu.Unlock()
	var ids []string
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
	return ids, pressed
}

func messageIDs(rows []db.DiscordCard) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.MessageID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

type attemptResult struct {
	Kind cardKind
	Live bool
	Err  error
}

type subjectPlan struct {
	row   db.DiscordCard
	gone  bool
	held  bool
	live  bool
	shown *card.Update
	store json.RawMessage
}

func (b *Bot) refreshMessage(ctx context.Context, d *db.DB, id string) attemptResult {
	rows, c, res, ok := b.loadMessage(ctx, d, id)
	if !ok {
		return res
	}
	plans := b.classify(ctx, d, c, rows)
	if allGone(plans) {
		if err := d.DeleteDiscordMessage(ctx, id); err != nil {
			b.log.WarnContext(ctx, "discord cards: drop gone message", "message", id, "error", err)
		}
		b.forget(id)
		return attemptResult{Kind: cardAllGone}
	}
	live := slices.ContainsFunc(plans, func(p subjectPlan) bool { return p.live })
	if slices.ContainsFunc(plans, func(p subjectPlan) bool { return p.held }) {
		return attemptResult{Kind: cardHeld, Live: live}
	}
	comps, hash, err := b.renderPlans(id, c, plans)
	if err != nil {
		return attemptResult{Kind: cardFailed, Live: live, Err: err}
	}
	res = b.sendAndStore(ctx, d, rows[0], hash, comps, storedUpdates(plans))
	res.Live = live
	return res
}

func (b *Bot) loadMessage(
	ctx context.Context, d *db.DB, id string,
) ([]db.DiscordCard, card.Card, attemptResult, bool) {
	var c card.Card
	rows, err := d.ListDiscordMessage(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			b.log.WarnContext(ctx, "discord cards: load message", "message", id, "error", err)
		}
		return nil, c, attemptResult{Kind: cardFailed, Err: err}, false
	}
	if len(rows) == 0 {
		b.forget(id)
		return nil, c, attemptResult{Kind: cardMissing}, false
	}
	if err = json.Unmarshal(rows[0].Card, &c); err != nil {
		return nil, c, attemptResult{Kind: cardUndecodable, Err: err}, false
	}
	return rows, c, attemptResult{}, true
}

func (b *Bot) classify(ctx context.Context, d *db.DB, c card.Card, rows []db.DiscordCard) []subjectPlan {
	asSent, _ := renderHash(render(c, nil, overlay{}, b.publicURL))
	plans := make([]subjectPlan, 0, len(rows))
	for _, r := range rows {
		plans = append(plans, b.planSubject(ctx, d, c, r, asSent))
	}
	return plans
}

func (b *Bot) planSubject(ctx context.Context, d *db.DB, c card.Card, r db.DiscordCard, asSent string) subjectPlan {
	last, known := b.decodeLast(ctx, r)
	p := subjectPlan{row: r, shown: sentVerbs(c, r.Subject, last)}
	if r.State == db.CardGone {
		p.gone = true
		return p
	}
	res := b.renderSubject(ctx, r.Subject)
	switch res.Outcome {
	case card.Render:
		kept := keepSentVerbs(c, r.Subject, res.Update)
		p.shown, p.live = &kept, res.Update.Live
		if j, err := json.Marshal(res.Update); err == nil && string(j) != string(r.LastUpdate) {
			p.store = j
		}
	case card.Gone:
		p.gone = true
		if err := d.MarkDiscordSubjectsGone(ctx, r.MessageID, []string{r.Subject}); err != nil {
			b.log.WarnContext(ctx, "discord cards: mark gone", "subject", r.Subject, "error", err)
		}
	case card.NotYet:
		switch {
		case known:
		case r.Hash == asSent:
			p.store = json.RawMessage("null")
		default:
			p.held = true
		}
	}
	return p
}

func (b *Bot) decodeLast(ctx context.Context, r db.DiscordCard) (*card.Update, bool) {
	if !r.LastKnown {
		return nil, false
	}
	if bytes.Equal(bytes.TrimSpace(r.LastUpdate), []byte("null")) {
		return nil, true
	}
	var u card.Update
	if err := json.Unmarshal(r.LastUpdate, &u); err != nil {
		if !b.cards.warned[r.MessageID] {
			b.cards.warned[r.MessageID] = true
			b.log.WarnContext(ctx, "discord cards: last_update undecodable", "message", r.MessageID,
				"subject", r.Subject, "error", err)
		}
		return nil, false
	}
	return &u, true
}

func sentVerbs(c card.Card, subject string, last *card.Update) *card.Update {
	if last == nil {
		return nil
	}
	kept := keepSentVerbs(c, subject, *last)
	return &kept
}

func allGone(plans []subjectPlan) bool {
	return !slices.ContainsFunc(plans, func(p subjectPlan) bool { return !p.gone })
}

func storedUpdates(plans []subjectPlan) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, p := range plans {
		if p.store != nil {
			out[p.row.Subject] = p.store
		}
	}
	return out
}

func (b *Bot) renderPlans(id string, c card.Card, plans []subjectPlan) ([]discordgo.MessageComponent, string, error) {
	ups := make(map[string]card.Update, len(plans))
	for _, p := range plans {
		if p.shown != nil {
			ups[p.row.Subject] = *p.shown
		}
	}
	ov := b.pinOverlay(id, ups)
	comps := render(c, ups, ov, b.publicURL)
	if textLen(comps) > card.MaxText {
		ups, ov = withinTextLimit(ups, ov)
		comps = render(c, ups, ov, b.publicURL)
	}
	hash, err := renderHash(comps)
	return comps, hash, err
}

func (b *Bot) sendAndStore(
	ctx context.Context, d *db.DB, first db.DiscordCard, hash string, comps []discordgo.MessageComponent,
	updates map[string]json.RawMessage,
) attemptResult {
	id := first.MessageID
	if hash != first.Hash {
		_, err := b.api.ChannelMessageEditComplex(
			&discordgo.MessageEdit{
				ID: id, Channel: first.ChannelID, Components: &comps, Flags: discordgo.MessageFlagsIsComponentsV2,
			},
			discordgo.WithContext(ctx), discordgo.WithRetryOnRatelimit(false),
		)
		var rest *discordgo.RESTError
		switch {
		case errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound:
			if err = d.DeleteDiscordMessage(ctx, id); err != nil {
				b.log.WarnContext(ctx, "discord cards: drop deleted message", "message", id, "error", err)
			}
			b.forget(id)
			return attemptResult{Kind: cardMissing}
		case err != nil:
			return attemptResult{Kind: cardFailed, Err: err}
		}
	} else if len(updates) == 0 {
		return attemptResult{Kind: cardShown}
	}
	if err := d.SetDiscordMessageRendered(ctx, id, hash, updates); err != nil {
		b.log.WarnContext(ctx, "discord cards: store render", "message", id, "error", err)
	}
	return attemptResult{Kind: cardShown}
}

func textLen(cs []discordgo.MessageComponent) int {
	n := 0
	for _, c := range cs {
		switch v := c.(type) {
		case discordgo.Container:
			n += textLen(v.Components)
		case discordgo.Section:
			n += textLen(v.Components)
		case discordgo.TextDisplay:
			n += utf8.RuneCountInString(v.Content)
		}
	}
	return n
}

func withinTextLimit(ups map[string]card.Update, ov overlay) (map[string]card.Update, overlay) {
	safe := func(bs []card.Button) []card.Button {
		return slices.DeleteFunc(slices.Clone(bs), func(bt card.Button) bool { return bt.Style == card.Danger })
	}
	out := make(map[string]card.Update, len(ups))
	for subject, up := range ups {
		if len(up.Sections) > 0 {
			up.Sections = []card.Field{{Name: "Details", Value: detailsTooLong}}
		}
		up.Buttons = safe(up.Buttons)
		out[subject] = up
	}
	ov.Confirm = safe(ov.Confirm)
	return out, ov
}

func (b *Bot) renderSubject(ctx context.Context, subject string) card.Rendering {
	r, ok := b.renderers[card.Kind(subject)]
	if !ok {
		return card.Rendering{}
	}
	res, err := r(ctx, subject)
	if err != nil {
		if ctx.Err() != nil {
			return card.Rendering{}
		}
		b.log.WarnContext(ctx, "discord cards: render", "subject", subject, "error", err)
		return card.Rendering{}
	}
	return res
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
	if ov, ok := b.cards.overlays[id]; ok {
		b.dropExpiredLocked(id, ov, b.now())
	}
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
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cards.overlays, id)
}

func (b *Bot) prune(ctx context.Context, d *db.DB) []string {
	ids, err := d.PruneDiscordCards(ctx, time.Now().Add(-cardKeep))
	if err != nil {
		b.log.WarnContext(ctx, "discord cards: prune", "error", err)
		return nil
	}
	for _, id := range ids {
		b.forget(id)
	}
	return ids
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
