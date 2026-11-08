package incident

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func (s *Service) AdvanceReporterProgress(ctx context.Context, now time.Time) {
	for _, status := range lifecycle.StatesWhere(func(m lifecycle.StateMeta) bool { return m.Progress }) {
		incs, err := s.db.FindByStatus(ctx, status)
		if err != nil {
			s.log.ErrorContext(ctx, "find incidents for progress", "status", status, "error", err)
			continue
		}
		for _, inc := range incs {
			s.reportProgress(ctx, inc, now)
		}
	}
}

func (s *Service) reportProgress(ctx context.Context, inc *db.Incident, now time.Time) {
	since, err := s.db.LastReporterNoteAt(ctx, inc.ID)
	if err != nil {
		s.log.ErrorContext(ctx, "read last reporter note", "incident", inc.ID, "error", err)
		return
	}
	if since.IsZero() {
		since = inc.CreatedAt
	}
	actions, err := s.db.ListActions(ctx, inc.ID)
	if err != nil {
		s.log.ErrorContext(ctx, "list actions for progress", "incident", inc.ID, "error", err)
		return
	}
	po, err := s.db.GetPendingOutcome(ctx, inc.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		s.log.ErrorContext(ctx, "get pending outcome for progress", "incident", inc.ID, "error", err)
	}
	phrases := progressPhrases(actions, since, po)
	if len(phrases) == 0 {
		return
	}
	s.noteReporters(ctx, inc,
		reporterCard(card.Info, fmt.Sprintf("⏳ Still working on **%s**: %s.", inc.Title, strings.Join(phrases, ", "))),
		now, s.minutes(s.timers(ctx).ProgressNoteGapMinutes))
}

func progressPhrases(actions []*db.ActionLog, since time.Time, po *db.PendingOutcome) []string {
	var out []string
	for _, a := range actions {
		if a.Status != db.ActionApplied || a.AppliedAt == nil || !a.AppliedAt.After(since) {
			continue
		}
		if p := agent.UserPhrase(a.Action); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if po != nil && po.LastStage == "downloading" && po.LastProgressAt.After(since) {
		out = append(out, fmt.Sprintf("downloading a new copy (%d%%)", int(po.LastProgressPct)))
	}
	return out
}

func (s *Service) armReporterNote(ctx context.Context, id string, now time.Time) {	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		s.log.ErrorContext(ctx, "load incident for progress", "incident", id, "error", err)
		return
	}
	if !lifecycle.Meta(inc.Status).Progress {
		return
	}
	last, err := s.db.LastReporterNoteAt(ctx, id)
	if err != nil {
		s.log.ErrorContext(ctx, "read last reporter note", "incident", id, "error", err)
		return
	}
	since := last
	if since.IsZero() {
		since = inc.CreatedAt
	}
	actions, err := s.db.ListActions(ctx, id)
	if err != nil {
		s.log.ErrorContext(ctx, "list actions for progress", "incident", id, "error", err)
		return
	}
	hasNew := false
	for _, a := range actions {
		if a.Status == db.ActionApplied && a.AppliedAt != nil && a.AppliedAt.After(since) &&
			agent.UserPhrase(a.Action) != "" {
			hasNew = true
			break
		}
	}
	if !hasNew {
		return
	}
	gap := s.minutes(s.timers(ctx).ProgressNoteGapMinutes)
	if !now.Before(last.Add(gap)) {
		s.reportProgress(ctx, inc, now)
		return
	}
	s.progressTimersMu.Lock()
	if prev, ok := s.progressTimers[id]; ok {
		prev.Stop()
	}
	s.progressTimers[id] = time.AfterFunc(time.Until(last.Add(gap)), func() {
		s.progressTimersMu.Lock()
		delete(s.progressTimers, id)
		s.progressTimersMu.Unlock()
		s.armReporterNote(s.work.base, id, time.Now())
	})
	s.progressTimersMu.Unlock()
}

func (s *Service) rearmReporterNotes(ctx context.Context) {
	now := time.Now()
	for _, status := range lifecycle.StatesWhere(func(m lifecycle.StateMeta) bool { return m.Progress }) {
		incs, err := s.db.FindByStatus(ctx, status)
		if err != nil {
			s.log.ErrorContext(ctx, "find incidents for progress", "status", status, "error", err)
			continue
		}
		for _, inc := range incs {
			s.armReporterNote(ctx, inc.ID, now)
		}
	}
}
