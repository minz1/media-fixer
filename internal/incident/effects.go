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

func (s *Service) decide(ev lifecycle.Event, l lifecycle.Limits, extra eventsFor, render db.Render) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		cur.In.Limits = l
		d, err := lifecycle.Next(cur, ev, time.Now())
		if err != nil {
			return db.Decision{}, err
		}
		out := db.Decision{Decision: d, Render: render}
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
	a, err := s.db.ApplyIncident(ctx, id, s.decide(ev, s.limits(t), extra, s.render(t, "")))
	if err != nil {
		if errors.Is(err, lifecycle.ErrNoSpec) {
			s.log.ErrorContext(ctx, "owner approve without an operation spec", "reject", "no_spec", "incident", id)
		}
		return a, err
	}
	s.afterCommit(ctx, a)
	if armsReporterNote(ev.Kind) && lifecycle.Meta(a.Snap.Status).Progress {
		s.armReporterNote(ctx, a.Snap.ID, time.Now())
	}
	return a, nil
}

func armsReporterNote(k lifecycle.EventKind) bool {
	return k == lifecycle.AttemptFinished || k == lifecycle.RunFinished || k == lifecycle.DownloadObserved
}

func (s *Service) afterCommit(ctx context.Context, a db.Applied) {
	s.runs.observe(a.Snap.ID, a.Snap.Generation)
	s.journal.Publish(a.Events...)
	if lifecycle.IsActive(a.PrevStatus) && !lifecycle.IsActive(a.Snap.Status) {
		s.endLockEpisode(ctx)
	}
	s.runEffects(ctx, a)
}

func (s *Service) runEffects(ctx context.Context, a db.Applied) {
	if len(a.Effects) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	notify := false
	for _, e := range a.Effects {
		notify = notify || isNotify(e.Kind)
	}
	if notify {
		s.outbox.Flush(ctx, time.Now())
	}
	var inc *db.Incident
	for _, e := range a.Effects {
		switch e.Kind {
		case lifecycle.LaunchRun:
			if inc == nil {
				var err error
				if inc, err = s.db.GetIncident(ctx, a.Snap.ID); err != nil {
					s.log.ErrorContext(ctx, "effects: load incident", "incident", a.Snap.ID, "error", err)
					return
				}
			}
			s.launchRun(inc, e)
		case lifecycle.CancelRun:
			s.runs.cancelBelow(a.Snap.ID, e.Gen)
		case lifecycle.ScheduleCheck:
			s.scheduleCheck(a.Snap.ID, e)
		case lifecycle.ExecutePlan, lifecycle.NotifyReporters, lifecycle.NotifyOwner,
			lifecycle.SeerrResolve, lifecycle.SeerrComment:
		}
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
	s.runs.armCheck(id, e.Gen, e.At, s.checkFire(id, e))
}

func (s *Service) scheduleCheckEarlier(id string, e lifecycle.Effect) {
	s.runs.armCheckEarlier(id, e.Gen, e.At, s.checkFire(id, e))
}

func (s *Service) checkFire(id string, e lifecycle.Effect) func(done func()) {
	gen, seq, kind := e.Gen, e.Seq, e.Check
	short := (&db.Incident{ID: id}).ShortID()
	return func(done func()) {
		if !s.work.goNamed("check:"+short, func(ctx context.Context) {
			defer done()
			s.runCheck(ctx, id, gen, seq, kind)
		}) {
			done()
			s.log.Info("check not started, shutting down", "incident", id, "check", string(kind))
		}
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

func downloadCard(inc *db.Incident, n lifecycle.Notice, t Timers) card.Card {
	body := ""
	if n.Problem == lifecycle.ProblemNoRelease || n.Problem == lifecycle.ProblemSearchEnded {
		body = "Approve \"Keep searching\" on the dashboard to keep watching for a release for up to " +
			KeepSearchingSpan(t.KeepSearchingDays) + ", or investigate manually."
	}
	c := ownerCard(inc, card.Warn, fmt.Sprintf("⚠️ Incident **%s** (#%s): searched for **%s** but %s.",
		inc.Title, inc.ShortID(), n.Target, downloadReason(n)), body)
	c.Verbs = escalationVerbs()
	return c
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
