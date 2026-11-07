package incident

import (
	"context"
	"fmt"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func (s *Service) startPendingOutcomeTracking(ctx context.Context, id string, gen int64, target *agent.SearchTarget) {
	ev := lifecycle.Event{Kind: lifecycle.RunFinished, Gen: gen, Outcome: lifecycle.Outcome{
		Kind: lifecycle.OutcomeAwaitDownload,
		Target: &lifecycle.Download{
			MediaType: target.MediaType, Title: target.Title, Season: target.Season, Episode: target.Episode,
		},
	}}
	s.applyOutcome(ctx, id, ev)
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
