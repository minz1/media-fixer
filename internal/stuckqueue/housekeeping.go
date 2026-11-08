package stuckqueue

import (
	"context"
	"errors"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

func (s *Scheduler) housekeep(now time.Time, timeout time.Duration) {
	release, err := s.claim(holderHousekeeping, now)
	if err != nil {
		return
	}
	effs := append(s.pollImports(s.base, now, timeout), s.expireActed(s.base, now)...)
	release(nil)
	s.runEffects(effs)
}

func (s *Scheduler) pollImports(ctx context.Context, now time.Time, timeout time.Duration) []Effect {
	rows, err := s.d.ListStuckInState(ctx, string(StateImportInflight))
	if err != nil {
		s.r.logger().WarnContext(ctx, "stuck-queue: listing running imports failed", "err", err)
		return nil
	}
	var effs []Effect
	for _, app := range allApps() {
		events := map[Key]Event{}
		for _, row := range rows {
			if App(row.App) != app || s.r.client(app) == nil {
				continue
			}
			cmd, ok := s.readCommand(ctx, s.r.client(app), row)
			if ok {
				events[Key{App: app, DownloadID: row.DownloadID}] = Event{
					Kind: EvCommandObserved, Cmd: cmd, Timeout: timeout, At: now,
				}
			}
		}
		effs = append(effs, s.applyHousekeeping(ctx, app, events)...)
	}
	return effs
}

func (s *Scheduler) readCommand(ctx context.Context, c *client.ArrClient, row db.StuckRow) (CmdStatus, bool) {
	status, err := c.Command(ctx, row.ImportCommandID)
	switch {
	case ctx.Err() != nil:
		return "", false
	case errors.Is(err, client.ErrNotFound):
		return CmdGone, true
	case err != nil:
		return CmdUnreadable, true
	}
	return cmdStatusOf(status), true
}

func cmdStatusOf(status string) CmdStatus {
	switch status {
	case "completed":
		return CmdCompleted
	case "failed", "aborted", "cancelled", "orphaned":
		return CmdFailed
	}
	return CmdRunning
}

func (s *Scheduler) expireActed(ctx context.Context, now time.Time) []Effect {
	rows, err := s.d.ListActedBefore(ctx, now.Add(-Keep))
	if err != nil {
		s.r.logger().WarnContext(ctx, "stuck-queue: listing old outcomes failed", "err", err)
		return nil
	}
	var effs []Effect
	for _, app := range allApps() {
		events := map[Key]Event{}
		for _, row := range rows {
			if App(row.App) == app {
				events[Key{App: app, DownloadID: row.DownloadID}] = Event{Kind: EvExpire, At: now}
			}
		}
		effs = append(effs, s.applyHousekeeping(ctx, app, events)...)
	}
	return effs
}

func (s *Scheduler) applyHousekeeping(ctx context.Context, app App, events map[Key]Event) []Effect {
	if len(events) == 0 {
		return nil
	}
	_, effs, err := s.r.apply(ctx, app, events)
	if err != nil {
		s.r.logger().WarnContext(ctx, "stuck-queue: housekeeping write failed", "app", app, "err", err)
		return nil
	}
	return effs
}
