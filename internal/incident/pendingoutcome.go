package incident

import (
	"context"
	"fmt"
	"slices"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func (s *Service) startPendingOutcomeTracking(
	ctx context.Context, inc *db.Incident, gen int64, result *agent.DiagnosticResult,
) {
	target := s.lastArrSearchMissingParams(ctx, inc.ID)
	if target == nil {
		s.runVerification(ctx, inc, gen, result)
		return
	}
	ev := lifecycle.Event{Kind: lifecycle.RunFinished, Gen: gen, Outcome: lifecycle.Outcome{
		Kind: lifecycle.OutcomeAwaitDownload,
		Target: &lifecycle.Download{
			MediaType: target.MediaType, Title: target.Title, Season: target.Season, Episode: target.Episode,
		},
	}}
	s.applyOutcome(ctx, inc.ID, ev)
}

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

func pendingOutcomeOf(d *lifecycle.Download) *db.PendingOutcome {
	return &db.PendingOutcome{
		MediaType: d.MediaType, Title: d.Title, Season: d.Season, Episode: d.Episode, StartedAt: d.StartedAt,
		LastStage: string(d.Stage), QueueStatus: d.QueueStatus, LastProgressAt: d.LastProgressAt,
		LastProgressPct: d.LastProgressPct, GrabNotified: d.GrabNotified, KeepSearching: d.KeepSearching,
		KeepSearchingUntil: d.KeepSearchingUntil,
	}
}

func (s *Service) downloadCheck(ctx context.Context, id string, gen int64) {
	snap, _, ok := s.checkTarget(ctx, id, gen, lifecycle.AwaitingDownload)
	obs := s.observer()
	if !ok || snap.Download == nil || obs == nil {
		return
	}
	ev := lifecycle.Event{Kind: lifecycle.DownloadObserved, Gen: gen}
	o, err := obs.CheckPendingOutcome(ctx, pendingOutcomeOf(snap.Download))
	if err != nil {
		s.log.WarnContext(ctx, "pending outcome check failed, will retry", "incident", id, "error", err)
		ev.Err = err.Error()
	} else {
		ev.Obs = &lifecycle.Observation{
			HasFile: o.HasFile, InQueue: o.QueueStage != "", QueueStatus: o.QueueStage, ProgressPct: o.ProgressPct,
		}
	}
	if _, err = s.apply(ctx, id, ev, nil); err != nil {
		s.logCheckErr(ctx, id, err)
	}
}

func KeepSearchingSpan(days int) string {
	if days == 1 {
		return "a day"
	}
	return fmt.Sprintf("%d days", days)
}
