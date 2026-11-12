package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/outbox"
)

var errSeerrOff = errors.New("seerr not configured")

const ownerTarget = "owner"

type outboxSender struct{ s *Service }

func (o outboxSender) NotifyOwner(ctx context.Context, c card.Card) error {
	return o.s.notif.NotifyOwner(ctx, c)
}

func (o outboxSender) NotifyUser(ctx context.Context, userID string, c card.Card) error {
	return o.s.notif.NotifyUser(ctx, userID, c)
}

func (o outboxSender) SeerrComment(ctx context.Context, issueID, text string) error {
	if o.s.seerr == nil {
		return errSeerrOff
	}
	return o.s.seerr.Comment(ctx, issueID, text)
}

func (o outboxSender) SeerrResolve(ctx context.Context, issueID string) error {
	if o.s.seerr == nil {
		return errSeerrOff
	}
	return o.s.seerr.Resolve(ctx, issueID)
}

func (s *Service) Outbox() *outbox.Dispatcher { return s.outbox }

func cardJSON(c card.Card) string {
	b, _ := json.Marshal(c)
	return string(b)
}

func ownerRow(key string, c card.Card) db.OutboxRow {
	return db.OutboxRow{Key: key, Target: ownerTarget, Kind: "card", Body: cardJSON(c)}
}

func (s *Service) reporterNotes(key string, c card.Card, gap time.Duration) []db.Note {
	notes := []db.Note{{Key: key, MinGap: gap, Body: cardJSON(c)}}
	if s.seerr != nil {
		notes = append(notes, db.Note{Key: key, Seerr: true, MinGap: gap, Body: c.Plain()})
	}
	return notes
}

func (s *Service) render(t Timers, seerrIssue string) (db.Render, func()) {
	var before, after int64
	r := func(inc *db.Incident, snap lifecycle.Snapshot, effects []lifecycle.Effect) db.Outgoing {
		var o db.Outgoing
		before = s.lockAlertAt.Load()
		base := fmt.Sprintf("inc:%s:g%d:v%d:%d", snap.ID, snap.Generation, snap.Version, time.Now().UnixNano())
		for i, e := range effects {
			s.renderEffect(&o, fmt.Sprintf("%s:%d", base, i), inc, snap, e, t, seerrIssue)
		}
		after = s.lockAlertAt.Load()
		return o
	}
	return r, func() {
		if after != before {
			s.lockAlertAt.CompareAndSwap(after, before)
		}
	}
}

func (s *Service) renderEffect(
	o *db.Outgoing,
	key string,
	inc *db.Incident,
	snap lifecycle.Snapshot,
	e lifecycle.Effect,
	t Timers,
	seerrIssue string,
) {
	switch e.Kind {
	case lifecycle.NotifyReporters:
		if c, gap, ok := s.reporterNote(inc, e, t); ok {
			o.Notes = append(o.Notes, s.reporterNotes(key, c, gap)...)
		}
	case lifecycle.NotifyOwner:
		if c, ok := s.ownerNotice(inc, snap, e.Notice, t); ok {
			o.Rows = append(o.Rows, ownerRow(key, c))
		}
	case lifecycle.SeerrResolve:
		if s.seerr != nil {
			o.Notes = append(o.Notes, db.Note{Key: key, Seerr: true, Resolve: true})
		}
	case lifecycle.SeerrComment:
		if s.seerr != nil && seerrIssue != "" {
			o.Notes = append(o.Notes, db.Note{Key: key, Seerr: true, To: seerrIssue,
				Body: fmt.Sprintf("👀 Thanks, we're looking into **%s**.", inc.Title)})
		}
	case lifecycle.LaunchRun, lifecycle.CancelRun, lifecycle.ScheduleCheck, lifecycle.ExecutePlan:
	}
}

func isNotify(k lifecycle.EffectKind) bool {
	return k == lifecycle.NotifyReporters || k == lifecycle.NotifyOwner ||
		k == lifecycle.SeerrResolve || k == lifecycle.SeerrComment
}

func (s *Service) reporterNote(inc *db.Incident, e lifecycle.Effect, t Timers) (card.Card, time.Duration, bool) {
	switch e.Note {
	case lifecycle.NoteFixed:
		body := fmt.Sprintf("✅ Your report for **%s** has been fixed automatically. Give it a try!", inc.Title)
		if result, ok := agent.ParseDiagnosticResult(inc.Finding); ok && inc.What == "subtitles" {
			if f, has := subtitleSearchField(result.SubtitleSearch); has && f.Name == fieldDownloaded {
				body += "\n" + f.Name + ": " + f.Value
			}
		}
		return reporterCard(card.Success, body), 0, true
	case lifecycle.NoteSearching:
		return reporterCard(card.Info, fmt.Sprintf(
			"🔎 Searching for **%s** — we'll let you know once it's downloading.", inc.Title)), 0, true
	case lifecycle.NoteFound:
		return reporterCard(card.Info, fmt.Sprintf(
			"📥 Found **%s** — downloading now. We'll let you know when it's ready.", inc.Title)), 0, true
	case lifecycle.NoteETA:
		return reporterCard(card.Info, fmt.Sprintf(
			"🔧 We're still rebuilding the library for **%s** — it should be playable in about %d minute(s). "+
				"Give it a try then!", inc.Title, e.ETAMinutes)), 0, true
	case lifecycle.NoteHanded:
		return reporterCard(card.Info,
			fmt.Sprintf("🧑‍🔧 Handed **%s** to minz1. You'll hear back here.", inc.Title)), 0, true
	case lifecycle.NoteAnotherLook:
		return reporterCard(card.Info, fmt.Sprintf("🔁 Taking another look at **%s**.", inc.Title)),
			s.minutes(t.RerunNoteGapMinutes), true
	case lifecycle.NoteResolved:
		return reporterCard(card.Success,
			fmt.Sprintf("✅ Your report for **%s** has been resolved. Give it a try!", inc.Title)), 0, true
	}
	return card.Card{}, 0, false
}

func (s *Service) ownerNotice(
	inc *db.Incident,
	snap lifecycle.Snapshot,
	n lifecycle.Notice,
	t Timers,
) (card.Card, bool) {
	var c card.Card
	switch n.Kind {
	case lifecycle.NoticeNone:
		return c, false
	case lifecycle.NoticeSystemic:
		return s.systemicAlert(inc, snap, t)
	case lifecycle.NoticeCard:
		fields := make([]card.Field, 0, len(n.Fields))
		for _, f := range n.Fields {
			fields = append(fields, card.Field{Name: f.Name, Value: f.Value})
		}
		c = ownerCard(inc, toneOf(n.Tone), n.Title, n.Body, fields...)
		c.Verbs = escalationVerbs()
	case lifecycle.NoticeStale:
		c = ownerCard(inc, card.Info, fmt.Sprintf(
			"⏱️ Incident **%s** (#%s) stopped making progress for %d min and is being re-run.",
			inc.Title, inc.ShortID(), n.Minutes), "")
		c.Verbs = []string{verbResolve}
	case lifecycle.NoticePlanInterrupted:
		c = ownerCard(inc, card.Warn, fmt.Sprintf(
			"⚠️ **%s** (#%s): the approved plan was interrupted by a restart.", inc.Title, inc.ShortID()), "")
		c.Verbs = escalationVerbs()
	case lifecycle.NoticeDownload:
		c = downloadCard(inc, n, t)
	}
	c.Subject = card.IncidentSubject(inc.ID)
	return c, true
}

func (s *Service) systemicAlert(inc *db.Incident, snap lifecycle.Snapshot, t Timers) (card.Card, bool) {
	gap := t.LockAlertGapMinutes
	now := time.Now().UnixNano()
	prev := s.lockAlertAt.Load()
	recent := prev != 0 && time.Duration(now-prev) < s.minutes(gap)
	if recent || !s.lockAlertAt.CompareAndSwap(prev, now) {
		s.log.Warn("systemic lock: incident blocked, owner already alerted",
			"incident", inc.ID, "title", inc.Title, "open", snap.In.ActiveCount)
		return card.Card{}, false
	}
	c := ownerCard(inc, card.Alert, fmt.Sprintf(
		"⚠️ %d open incidents — possible systemic failure. Autonomous actions locked. New incident: **%s** (#%s). "+
			"Later incidents in this lock are blocked without a DM for the next %s; see the dashboard.",
		snap.In.ActiveCount+1, inc.Title, inc.ShortID(), gapText(gap)), "")
	c.Subject, c.Verbs = card.IncidentSubject(inc.ID), []string{verbUnlock, verbResolve}
	return c, true
}

func (s *Service) queueNotes(ctx context.Context, id string, now time.Time, o db.Outgoing) {
	if err := s.db.QueueOutgoing(ctx, id, now, o); err != nil {
		s.log.ErrorContext(ctx, "queue reporter note", "incident", id, "error", err)
		return
	}
	s.outbox.Flush(ctx, time.Now())
}

func (s *Service) notifyReporters(ctx context.Context, inc *db.Incident, c card.Card) {
	now := time.Now()
	s.queueNotes(
		ctx,
		inc.ID,
		now,
		db.Outgoing{Notes: s.reporterNotes(fmt.Sprintf("note:%s:%d", inc.ID, now.UnixNano()), c, 0)},
	)
}

func (s *Service) outboxFailed(ctx context.Context, r db.OutboxRow, now time.Time, sendErr error) bool {
	scheme, to, _ := strings.Cut(r.Target, ":")
	if r.IncidentID == "" || (scheme != "discord" && scheme != "seerr") {
		return false
	}
	inc, err := s.db.GetIncident(ctx, r.IncidentID)
	if err != nil {
		s.log.ErrorContext(ctx, "outbox fallback: load incident", "incident", r.IncidentID, "error", err)
		return false
	}
	var c card.Card
	key := "reporter-fallback:" + to + ":" + now.Format(time.DateOnly)
	if scheme == "discord" {
		var msg card.Card
		_ = json.Unmarshal([]byte(r.Body), &msg)
		c = ownerCard(inc, card.Warn, fmt.Sprintf("⚠️ Could not DM reporter %s for **%s** (#%s): %v",
			to, inc.Title, inc.ShortID(), sendErr), "", card.Field{Name: "Message was", Value: msg.Plain()})
	} else {
		t := s.timers(ctx)
		if _, ok := s.claimSeerrAlert(now, s.minutes(t.SeerrAlertGapMinutes)); !ok {
			return false
		}
		verb := "comment on"
		if r.Kind == "seerr_resolve" {
			verb = "resolve"
		}
		key = fmt.Sprintf("seerr-alert:%d", now.UnixNano())
		c = ownerCard(inc, card.Warn, fmt.Sprintf("⚠️ Could not %s Seerr issue %s for **%s** (#%s): %v",
			verb, to, inc.Title, inc.ShortID(), sendErr),
			"Further Seerr failures in the next "+gapText(t.SeerrAlertGapMinutes)+" are only logged.")
	}
	row := ownerRow(key, c)
	row.IncidentID, row.CreatedAt, row.NextAt = inc.ID, now, now
	if err = s.db.EnqueueOutbox(ctx, row); err != nil {
		s.log.ErrorContext(ctx, "outbox fallback: queue owner card", "incident", inc.ID, "error", err)
		return false
	}
	return true
}
func gapText(minutes int) string {
	if minutes%minutesPerHour != 0 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	if h := minutes / minutesPerHour; h != 1 {
		return fmt.Sprintf("%d hours", h)
	}
	return "hour"
}

func (s *Service) claimSeerrAlert(now time.Time, gap time.Duration) (time.Time, bool) {
	s.seerrAlertMu.Lock()
	defer s.seerrAlertMu.Unlock()
	prev := s.seerrAlertAt
	if now.Sub(prev) < gap {
		return prev, false
	}
	s.seerrAlertAt = now
	return prev, true
}
