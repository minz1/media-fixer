package incident

import (
	"context"
	"fmt"
	"strings"
	"time"

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

func (s *Service) ArmSonarrDownloadWake(ctx context.Context, mediaType, title string, season, episode int) error {
	incs, err := s.db.FindByStatus(ctx, lifecycle.AwaitingDownload)
	if err != nil {
		return err
	}
	for _, inc := range incs {
		snap, loadErr := s.db.GetIncidentSnapshot(ctx, inc.ID)
		if loadErr != nil {
			s.log.ErrorContext(ctx, "sonarr wake: load incident", "incident", inc.ID, "error", loadErr)
			continue
		}
		d := snap.Download
		if d == nil || d.MediaType != mediaType || !strings.EqualFold(d.Title, title) ||
			(d.Season >= 0 && d.Season != season) || (d.Episode >= 0 && d.Episode != episode) {
			continue
		}
		s.scheduleCheckEarlier(inc.ID, lifecycle.Effect{
			Kind: lifecycle.ScheduleCheck, Gen: snap.Generation, At: time.Now(), Check: lifecycle.CheckDownload,
		})
	}
	return nil
}
