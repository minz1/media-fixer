package incident

import (
	"context"
	"errors"
	"fmt"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

const (
	planTTLMinutes   = 60
	extensionsMax    = 3
	stuckReadMinutes = stuckReadVerifyFloorMinutes
)

type eventsFor func(cur lifecycle.Snapshot, d lifecycle.Decision) []*db.Event

func (s *Service) limits(t Timers) lifecycle.Limits {
	return lifecycle.Limits{
		MaxActions:         agent.MaxAutonomousActions,
		SystemicThreshold:  SystematicIncidentThreshold,
		VerifyChecks:       t.VerifyChecks,
		ExtensionsMax:      extensionsMax,
		Minute:             s.minutes(1),
		FirstVerifyCap:     s.minutes(t.FirstVerifyCapMinutes),
		VerifyGap:          s.minutes(t.VerifyGapMinutes),
		StuckReadFloor:     s.minutes(stuckReadMinutes),
		ApprovedVerify:     s.minutes(t.ApprovedVerifyMinutes),
		CheckEvery:         s.minutes(t.CheckEveryMinutes),
		NoReleaseGrace:     s.minutes(t.NoReleaseGraceMinutes),
		Stall:              s.minutes(t.StallMinutes),
		TrackCap:           s.minutes(minutesPerHour * t.TrackCapHours),
		KeepSearchingCheck: s.minutes(minutesPerHour * t.KeepSearchingCheckHours),
		KeepSearchingSpan:  s.minutes(minutesPerDay * t.KeepSearchingDays),
		PlanTTL:            s.minutes(planTTLMinutes),
	}
}

func (s *Service) decide(ev lifecycle.Event, l lifecycle.Limits, extra eventsFor) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		cur.In.Limits = l
		d, err := lifecycle.Next(cur, ev, time.Now())
		if err != nil {
			return db.Decision{}, err
		}
		out := db.Decision{Decision: d}
		if extra != nil {
			out.Events = extra(cur, d)
		}
		return out, nil
	}
}

func (s *Service) apply(ctx context.Context, id string, ev lifecycle.Event, extra eventsFor) (db.Applied, error) {
	release := s.runs.reserve(id)
	defer release()
	t := s.timers(ctx)
	a, err := s.db.ApplyIncident(ctx, id, s.decide(ev, s.limits(t), extra))
	if err != nil {
		if errors.Is(err, lifecycle.ErrNoSpec) {
			s.log.ErrorContext(ctx, "owner approve without an operation spec", "reject", "no_spec", "incident", id)
		}
		return a, err
	}
	s.afterCommit(ctx, a, t, "")
	return a, nil
}

func (s *Service) afterCommit(ctx context.Context, a db.Applied, t Timers, seerrIssue string) {
	s.runs.observe(a.Snap.ID, a.Snap.Generation)
	s.journal.Publish(a.Events...)
	s.runEffects(ctx, a, t, seerrIssue)
}

type effectRun struct {
	s          *Service
	a          db.Applied
	t          Timers
	inc        *db.Incident
	seerrIssue string
}

func (s *Service) runEffects(ctx context.Context, a db.Applied, t Timers, seerrIssue string) {
	if len(a.Effects) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	r := &effectRun{s: s, a: a, t: t, seerrIssue: seerrIssue}
	inc, err := s.db.GetIncident(ctx, a.Snap.ID)
	if err != nil {
		s.log.ErrorContext(ctx, "effects: load incident", "incident", a.Snap.ID, "error", err)
		return
	}
	r.inc = inc
	for _, e := range a.Effects {
		r.run(ctx, e)
	}
}

func (r *effectRun) run(ctx context.Context, e lifecycle.Effect) {
	s, id := r.s, r.a.Snap.ID
	switch e.Kind {
	case lifecycle.LaunchRun:
		s.launchRun(r.inc, e)
	case lifecycle.CancelRun:
		s.runs.cancelBelow(id, e.Gen)
	case lifecycle.ScheduleCheck:
		s.scheduleCheck(id, e)
	case lifecycle.ExecutePlan:
	case lifecycle.NotifyReporters:
		r.notifyReporters(ctx, e)
	case lifecycle.NotifyOwner:
		r.notifyOwner(ctx, e.Notice)
	case lifecycle.SeerrResolve:
		s.resolveSeerrIssues(ctx, r.inc)
	case lifecycle.SeerrComment:
		r.seerrComment(ctx)
	}
}

func (s *Service) launchRun(inc *db.Incident, e lifecycle.Effect) {
	runCtx, tok, ok := s.runs.supersedeGen(inc.ID, e.Gen)
	if !ok {
		s.log.Info("run not launched, a newer generation holds the incident", "incident", inc.ID, "gen", e.Gen)
		return
	}
	seed := func(context.Context) []openai.ChatCompletionMessage { return nil }
	if e.Seed == lifecycle.SeedSummary {
		seed = func(seedCtx context.Context) []openai.ChatCompletionMessage {
			return s.buildReinvestigateSeed(seedCtx, inc.ID, inc)
		}
	}
	s.startRun(runCtx, tok, inc, e.Gen, seed)
}

func (s *Service) scheduleCheck(id string, e lifecycle.Effect) {
	gen, kind := e.Gen, e.Check
	short := (&db.Incident{ID: id}).ShortID()
	s.runs.armCheck(id, gen, e.At, func(done func()) {
		if !s.work.goNamed("check:"+short, func(ctx context.Context) {
			defer done()
			s.runCheck(ctx, id, gen, kind)
		}) {
			done()
			s.log.Info("check not started, shutting down", "incident", id, "check", string(kind))
		}
	})
}

func (r *effectRun) notifyReporters(ctx context.Context, e lifecycle.Effect) {
	s, inc := r.s, r.inc
	switch e.Note {
	case lifecycle.NoteFixed:
		s.notifyReporters(ctx, inc, reporterCard(card.Success,
			fmt.Sprintf("✅ Your report for **%s** has been fixed automatically. Give it a try!", inc.Title)))
	case lifecycle.NoteSearching:
		s.notifyReporters(ctx, inc, reporterCard(card.Info, fmt.Sprintf(
			"🔎 Searching for **%s** — we'll let you know once it's downloading.", inc.Title)))
	case lifecycle.NoteFound:
		s.notifyReporters(ctx, inc, reporterCard(card.Info, fmt.Sprintf(
			"📥 Found **%s** — downloading now. We'll let you know when it's ready.", inc.Title)))
	case lifecycle.NoteETA:
		s.notifyReporters(ctx, inc, reporterCard(card.Info, fmt.Sprintf(
			"🔧 We're still rebuilding the library for **%s** — it should be playable in about %d minute(s). "+
				"Give it a try then!", inc.Title, e.ETAMinutes)))
	case lifecycle.NoteHanded:
		s.notifyReporters(ctx, inc, reporterCard(card.Info,
			fmt.Sprintf("🧑‍🔧 Handed **%s** to minz1. You'll hear back here.", inc.Title)))
	case lifecycle.NoteAnotherLook:
		s.noteReporters(ctx, inc, reporterCard(card.Info, fmt.Sprintf("🔁 Taking another look at **%s**.", inc.Title)),
			time.Now(), s.minutes(r.t.RerunNoteGapMinutes))
	case lifecycle.NoteResolved:
		s.notifyReporters(ctx, inc, reporterCard(card.Success,
			fmt.Sprintf("✅ Your report for **%s** has been resolved. Give it a try!", inc.Title)))
	}
}

func (r *effectRun) notifyOwner(ctx context.Context, n lifecycle.Notice) {
	s, inc := r.s, r.inc
	var c card.Card
	switch n.Kind {
	case lifecycle.NoticeNone:
		return
	case lifecycle.NoticeSystemic:
		r.systemicAlert(ctx)
		return
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
		c = r.downloadCard(n)
	}
	c.Subject = card.IncidentSubject(inc.ID)
	if err := s.notif.NotifyOwner(ctx, c); err != nil && !errors.Is(err, card.ErrUntracked) {
		s.log.ErrorContext(ctx, "notify owner", "incident", inc.ID, "error", err)
	}
}

func downloadReason(n lifecycle.Notice) string {
	switch n.Problem {
	case lifecycle.ProblemLostTrack:
		return "we lost track of the download and could not re-check its state: " + n.Err
	case lifecycle.ProblemStalled:
		return "the download appears to have stalled"
	case lifecycle.ProblemOverlong:
		return "the download has been running far longer than expected"
	case lifecycle.ProblemNeverImported:
		return "it was grabbed but never imported"
	case lifecycle.ProblemSearchEnded:
		return "still no release found after an extended search"
	case lifecycle.ProblemNoRelease, lifecycle.ProblemNone:
	}
	return "no release was found"
}

func (r *effectRun) downloadCard(n lifecycle.Notice) card.Card {
	inc := r.inc
	body := ""
	if n.Problem == lifecycle.ProblemNoRelease || n.Problem == lifecycle.ProblemSearchEnded {
		body = "Approve \"Keep searching\" on the dashboard to keep watching for a release for up to " +
			KeepSearchingSpan(r.t.KeepSearchingDays) + ", or investigate manually."
	}
	c := ownerCard(inc, card.Warn, fmt.Sprintf("⚠️ Incident **%s** (#%s): searched for **%s** but %s.",
		inc.Title, inc.ShortID(), n.Target, downloadReason(n)), body)
	c.Verbs = escalationVerbs()
	return c
}

func (r *effectRun) systemicAlert(ctx context.Context) {
	s, inc := r.s, r.inc
	gap := r.t.LockAlertGapMinutes
	now := time.Now().UnixNano()
	prev := s.lockAlertAt.Load()
	recent := prev != 0 && time.Duration(now-prev) < s.minutes(gap)
	if recent || !s.lockAlertAt.CompareAndSwap(prev, now) {
		s.log.WarnContext(ctx, "systemic lock: incident blocked, owner already alerted",
			"incident", inc.ID, "title", inc.Title, "open", r.a.Snap.In.ActiveCount)
		return
	}
	c := ownerCard(inc, card.Alert, fmt.Sprintf(
		"⚠️ %d open incidents — possible systemic failure. Autonomous actions locked. New incident: **%s** (#%s). "+
			"Later incidents in this lock are blocked without a DM for the next %s; see the dashboard.",
		r.a.Snap.In.ActiveCount+1, inc.Title, inc.ShortID(), gapText(gap)), "")
	c.Subject, c.Verbs = card.IncidentSubject(inc.ID), []string{verbUnlock, verbResolve}
	if err := s.notif.NotifyOwner(ctx, c); err != nil && !errors.Is(err, card.ErrUntracked) {
		s.lockAlertAt.CompareAndSwap(now, 0)
		s.log.ErrorContext(ctx, "notify owner", "error", err)
	}
}

func (r *effectRun) seerrComment(ctx context.Context) {
	s, inc := r.s, r.inc
	if s.seerr == nil || r.seerrIssue == "" {
		return
	}
	msg := fmt.Sprintf("👀 Thanks, we're looking into **%s**.", inc.Title)
	s.callSeerrIssue(ctx, inc, "comment on", r.seerrIssue, func(id string) error {
		return s.seerr.Comment(ctx, id, msg)
	})
	s.claimReporterNote(ctx, inc.ID, time.Now(), 0)
}

const (
	toneInfo    = "info"
	toneSuccess = "success"
	toneWarn    = "warn"
	toneAlert   = "alert"
)

func toneOf(s string) card.Tone {
	switch s {
	case toneSuccess:
		return card.Success
	case toneWarn:
		return card.Warn
	case toneAlert:
		return card.Alert
	}
	return card.Info
}
func toneName(t card.Tone) string {
	switch t {
	case card.Success:
		return toneSuccess
	case card.Warn:
		return toneWarn
	case card.Alert:
		return toneAlert
	case card.Info:
	}
	return toneInfo
}
func noticeOf(c card.Card) lifecycle.Notice {
	n := lifecycle.Notice{Kind: lifecycle.NoticeCard, Tone: toneName(c.Tone), Title: c.Title, Body: c.Body}
	for _, f := range c.Fields {
		n.Fields = append(n.Fields, lifecycle.Field{Name: f.Name, Value: f.Value})
	}
	return n
}
