package incident

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
)

// startPendingOutcomeTracking begins durable tracking for an arr_search_missing
// fix: unlike the generic runVerification loop (an in-memory goroutine capped
// at ~10 minutes total), this survives a process restart and can track a
// download for hours, because it's driven by the same background sweeper as
// SweepStaleRuns rather than a goroutine sleeping in this call stack.
//
// The search target's params come from the just-logged actions_log entry
// (Dispatcher.logAction already recorded media_type/title/season/episode when
// the tool ran) rather than complete_diagnosis's own arguments — the model
// never has to restate them, and there's no risk of the two disagreeing.
func (s *Service) startPendingOutcomeTracking(ctx context.Context, inc *db.Incident, result *agent.DiagnosticResult) {
	target := s.lastArrSearchMissingParams(ctx, inc.ID)
	if target == nil {
		// Couldn't recover what was searched for (shouldn't happen in
		// practice — the action was just applied this same run — but fail
		// safe into the generic verify loop rather than silently doing
		// nothing).
		s.runVerification(ctx, inc, result)
		return
	}

	changed, err := s.db.TransitionStatus(ctx, inc.ID, db.StatusVerifying,
		db.StatusOpen, db.StatusInvestigating, db.StatusReopened, db.StatusManualTestNeeded)
	if err != nil {
		s.log.ErrorContext(ctx, "enter verifying transition (pending outcome)", "incident", inc.ID, "error", err)
		return
	}
	if !changed {
		s.log.InfoContext(ctx, "not entering pending outcome tracking, incident already progressed", "incident", inc.ID)
		return
	}
	s.recordStatusChanged(ctx, inc.ID, string(db.StatusVerifying))

	t := s.timers(ctx)
	now := time.Now()
	po := &db.PendingOutcome{
		MediaType:      target.MediaType,
		Title:          target.Title,
		Season:         target.Season,
		Episode:        target.Episode,
		StartedAt:      now,
		LastStage:      stageSearching,
		LastProgressAt: now,
	}
	if setErr := s.db.SetPendingOutcome(ctx, inc.ID, po, now.Add(s.minutes(t.CheckEveryMinutes))); setErr != nil {
		s.log.ErrorContext(ctx, "set pending outcome", "incident", inc.ID, "error", setErr)
		return
	}
	s.notifyReporters(ctx, inc, reporterCard(card.Info, fmt.Sprintf(
		"🔎 Searching for **%s** — we'll let you know once it's downloading.", inc.Title,
	)))
}

const stageSearching = "searching"

// arrSearchTarget is what startPendingOutcomeTracking recovers from
// actions_log to seed a PendingOutcome.
type arrSearchTarget struct {
	MediaType string
	Title     string
	Season    int
	Episode   int
}

// lastArrSearchMissingParams finds the most recently logged arr_search_missing
// action on this incident and decodes its params. Returns nil if none is found
// (defensive — startPendingOutcomeTracking is only called right after one ran).
func (s *Service) lastArrSearchMissingParams(ctx context.Context, incidentID string) *arrSearchTarget {
	actions, err := s.db.ListActions(ctx, incidentID)
	if err != nil {
		return nil
	}
	for _, v := range slices.Backward(actions) {
		if v.Action != agent.ToolArrSearchMissing {
			continue
		}
		params, ok := v.Params.(map[string]any)
		if !ok {
			return nil
		}
		return &arrSearchTarget{
			MediaType: fmt.Sprint(params["media_type"]),
			Title:     fmt.Sprint(params["title"]),
			Season:    intFromAny(params["season"], -1),
			Episode:   intFromAny(params["episode"], -1),
		}
	}
	return nil
}

// intFromAny extracts an int from a value that round-tripped through
// JSON (so a number decodes as float64), returning def if v is absent, not a
// number, or NaN-shaped in some way json wouldn't produce.
func intFromAny(v any, def int) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return def
}

// AdvancePendingOutcomes polls live *arr state for every incident with a due
// pending outcome and advances each one. Called from the same background
// sweep loop as SweepStaleRuns (see NewService), not a second ticker.
func (s *Service) AdvancePendingOutcomes(ctx context.Context) {
	if s.agent == nil {
		return
	}
	due, err := s.db.FindDuePendingOutcomes(ctx, time.Now())
	if err != nil {
		s.log.ErrorContext(ctx, "pending outcome sweep query", "error", err)
		return
	}
	t := s.timers(ctx)
	for _, inc := range due {
		s.advancePendingOutcome(ctx, inc, t)
	}
}

// advancePendingOutcome polls once for inc's pending outcome and decides what
// happens next: resolved (mark fixed), actively downloading (notify on the
// grab milestone, keep polling), stalled/capped/no-release (escalate), or
// just not due yet (reschedule).
func (s *Service) advancePendingOutcome(ctx context.Context, inc *db.Incident, t Timers) {
	po, err := s.db.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		return
	}

	obs, err := s.agent.CheckPendingOutcome(ctx, po)
	if err != nil {
		// A transient *arr failure is retried, but the cap checks below are skipped on this path, so bound it here.
		capHours := t.TrackCapHours
		if po.KeepSearching && !po.GrabNotified {
			capHours += t.KeepSearchingCheckHours
		}
		if time.Since(po.StartedAt) > s.minutes(minutesPerHour*capHours) {
			s.escalatePendingOutcome(ctx, inc, po,
				"we lost track of the download and could not re-check its state: "+err.Error(), t)
			return
		}
		s.log.WarnContext(ctx, "pending outcome check failed, will retry", "incident", inc.ID, "error", err)
		_ = s.db.SetPendingOutcome(ctx, inc.ID, po, time.Now().Add(s.minutes(t.CheckEveryMinutes)))
		return
	}

	if obs.HasFile {
		_ = s.db.ClearPendingOutcome(ctx, inc.ID)
		s.markFixedAndNotify(ctx, inc, agent.ToolArrSearchMissing)
		return
	}

	now := time.Now()
	if obs.QueueStage != "" {
		s.advanceDownloading(ctx, inc, po, obs, now, t)
		return
	}
	s.advanceNoQueueItem(ctx, inc, po, now, t)
}

// advanceDownloading handles the case where something is actively in the
// download queue for this target: a one-time "found it" DM on first sighting,
// progress tracking to distinguish real movement from a stall, and the
// overall time cap.
func (s *Service) advanceDownloading(
	ctx context.Context,
	inc *db.Incident,
	po *db.PendingOutcome,
	obs *agent.PendingOutcomeObservation,
	now time.Time,
	t Timers,
) {
	if !po.GrabNotified {
		po.GrabNotified = true
		if po.KeepSearching {
			po.StartedAt, po.LastProgressAt = now, now
		}
		s.notifyReporters(ctx, inc, reporterCard(card.Info, fmt.Sprintf(
			"📥 Found **%s** — downloading now. We'll let you know when it's ready.", inc.Title,
		)))
	}
	if obs.ProgressPct > po.LastProgressPct {
		po.LastProgressAt = now
	}
	po.LastStage, po.LastProgressPct = obs.QueueStage, obs.ProgressPct

	if now.Sub(po.LastProgressAt) > s.minutes(t.StallMinutes) {
		s.escalatePendingOutcome(ctx, inc, po, "the download appears to have stalled", t)
		return
	}
	if now.Sub(po.StartedAt) > s.minutes(minutesPerHour*t.TrackCapHours) {
		s.escalatePendingOutcome(ctx, inc, po, "the download has been running far longer than expected", t)
		return
	}
	_ = s.db.SetPendingOutcome(ctx, inc.ID, po, now.Add(s.minutes(t.CheckEveryMinutes)))
}

// advanceNoQueueItem handles the case where nothing is currently in the
// download queue for this target: either still waiting on an indexer within
// grace, genuinely no release found, or (if an owner opted in) a slower
// long-term watch for new content.
func (s *Service) advanceNoQueueItem(
	ctx context.Context, inc *db.Incident, po *db.PendingOutcome, now time.Time, t Timers,
) {
	po.LastStage = stageSearching
	if po.KeepSearching {
		if now.After(po.KeepSearchingUntil) {
			s.escalatePendingOutcome(ctx, inc, po, "still no release found after an extended search", t)
			return
		}
		resetKeepSearchingTrack(po, now)
		_ = s.db.SetPendingOutcome(ctx, inc.ID, po, now.Add(s.minutes(minutesPerHour*t.KeepSearchingCheckHours)))
		return
	}
	if now.Sub(po.StartedAt) > s.minutes(t.NoReleaseGraceMinutes) {
		s.escalatePendingOutcome(ctx, inc, po, "no release was found", t)
		return
	}
	_ = s.db.SetPendingOutcome(ctx, inc.ID, po, now.Add(s.minutes(t.CheckEveryMinutes)))
}

// escalatePendingOutcome hands a pending outcome to the owner. Deliberately
// does NOT clear pending_outcome — Service.KeepSearching reuses it to re-arm
// tracking, resetting its timers and progress — but FindDuePendingOutcomes is
// scoped to StatusVerifying, so the sweeper stops touching it the moment
// escalateToOwner moves status to manual_test_needed.
func (s *Service) escalatePendingOutcome(
	ctx context.Context,
	inc *db.Incident,
	po *db.PendingOutcome,
	reason string,
	t Timers,
) {
	s.log.WarnContext(ctx, "pending outcome escalating", "incident", inc.ID, "reason", reason)
	s.escalateToOwner(ctx, inc, ownerCard(inc, card.Warn, fmt.Sprintf(
		"⚠️ Incident **%s** (#%s): searched for **%s** but %s.",
		inc.Title, inc.ID[:8], pendingOutcomeTargetLabel(po), reason,
	), "Approve \"Keep searching\" on the dashboard to keep watching for a release over the next "+
		keepSearchingSpan(t.KeepSearchingDays)+", or investigate manually."))
}

// pendingOutcomeTargetLabel renders a PendingOutcome's target for a
// human-facing message, e.g. "Rick and Morty S09E09" or just "Arrival" for a
// movie/whole-series search.
func pendingOutcomeTargetLabel(po *db.PendingOutcome) string {
	switch {
	case po.Season >= 0 && po.Episode >= 0:
		return fmt.Sprintf("%s S%02dE%02d", po.Title, po.Season, po.Episode)
	case po.Season >= 0:
		return fmt.Sprintf("%s S%02d", po.Title, po.Season)
	default:
		return po.Title
	}
}

func keepSearchingSpan(days int) string {
	if days == 1 {
		return "day"
	}
	return fmt.Sprintf("%d days", days)
}

func resetKeepSearchingTrack(po *db.PendingOutcome, now time.Time) {
	po.StartedAt, po.LastProgressAt = now, now
	po.LastProgressPct = 0
	po.GrabNotified = false
	po.LastStage = stageSearching
}

// KeepSearching re-arms a pending outcome that was escalated because no
// release was found yet, so the sweeper resumes checking (at a slower
// cadence, see KeepSearchingCheckHours) instead of the incident
// sitting idle in manual_test_needed until an owner manually reruns it.
func (s *Service) KeepSearching(ctx context.Context, id string) error {
	po, err := s.db.GetPendingOutcome(ctx, id)
	if err != nil {
		return fmt.Errorf("keep searching: %w", err)
	}
	t := s.timers(ctx)
	resetKeepSearchingTrack(po, time.Now())
	po.KeepSearching = true
	po.KeepSearchingUntil = time.Now().Add(s.minutes(minutesPerDay * t.KeepSearchingDays))

	changed, err := s.db.TransitionStatus(ctx, id, db.StatusVerifying, db.StatusManualTestNeeded)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("incident %s is not awaiting a keep-searching decision", id)
	}
	s.recordStatusChanged(ctx, id, string(db.StatusVerifying))
	return s.db.SetPendingOutcome(ctx, id, po, time.Now().Add(s.minutes(minutesPerHour*t.KeepSearchingCheckHours)))
}
