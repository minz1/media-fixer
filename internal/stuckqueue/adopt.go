package stuckqueue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
)

func (s *Scheduler) Adopt(ctx context.Context) error {
	release, err := s.claim(holderHousekeeping, s.r.Now())
	if err != nil {
		return err
	}
	defer func() { release(nil) }()
	ops, err := s.d.ResumableDownloadOperations(ctx)
	if err != nil {
		return err
	}
	byApp := s.adoptable(ctx, ops, s.r.Now())
	var effs []Effect
	adopted := 0
	for _, app := range allApps() {
		n, e, adoptErr := s.adoptApp(ctx, app, byApp[app])
		if adoptErr != nil {
			return adoptErr
		}
		adopted, effs = adopted+n, append(effs, e...)
	}
	caught, more := s.catchUp(ctx)
	msg := fmt.Sprintf("stuck-queue: adopted %d re-grab ops, caught up %d", adopted, caught)
	if adopted > 0 {
		s.r.logger().WarnContext(ctx, msg)
	} else {
		s.r.logger().InfoContext(ctx, msg)
	}
	s.runEffects(append(effs, more...))
	return nil
}

func (s *Scheduler) adoptable(ctx context.Context, ops []operation.Op, now time.Time) map[App][]operation.Op {
	out := make(map[App][]operation.Op)
	for _, op := range ops {
		if now.Sub(op.UpdatedAt) >= Keep || len(op.Steps) == 0 {
			continue
		}
		app := App(op.Steps[0].App)
		if !slices.Contains(allApps(), app) {
			s.r.logger().WarnContext(ctx, "stuck-queue: re-grab op names an unknown app, not adopted",
				"op", op.ID, "app", op.Steps[0].App)
			continue
		}
		out[app] = append(out[app], op)
	}
	return out
}

func (s *Scheduler) adoptApp(ctx context.Context, app App, ops []operation.Op) (int, []Effect, error) {
	if len(ops) == 0 {
		return 0, nil, nil
	}
	adopted := 0
	var effs []Effect
	err := s.d.ApplyStuck(ctx, string(app), nil, func(cur map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
		events := map[Key]Event{}
		var keys []string
		for _, op := range ops {
			id := storedID(cur, op.Steps[0].DownloadID)
			if row, ok := cur[id]; ok && row.State == string(StateImportInflight) {
				s.r.logger().WarnContext(ctx, "stuck-queue: re-grab op not adopted, an import is running",
					"op", op.ID, "app", app, "download", id)
				continue
			}
			events[Key{App: app, DownloadID: id}] = Event{Kind: EvAdopted, OpID: op.ID, At: op.UpdatedAt}
			keys = append(keys, id)
		}
		slices.Sort(keys)
		var put []db.StuckRow
		var del []string
		_, effs, put, del = s.r.stepRows(ctx, app, cur, keys, events)
		adopted = len(put)
		return put, del, nil
	})
	return adopted, effs, err
}

func storedID(cur map[string]*db.StuckRow, downloadID string) string {
	if _, ok := cur[downloadID]; ok {
		return downloadID
	}
	for id := range cur {
		if strings.EqualFold(id, downloadID) {
			return id
		}
	}
	return downloadID
}

func (s *Scheduler) catchUp(ctx context.Context) (int, []Effect) {
	caught := 0
	var effs []Effect
	for _, app := range allApps() {
		rows, err := s.d.ListStuck(ctx, string(app))
		if err != nil {
			s.r.logger().WarnContext(ctx, "stuck-queue: catch-up could not list rows", "app", app, "err", err)
			continue
		}
		events := map[Key]Event{}
		stamped := map[Key]time.Time{}
		for _, row := range rows {
			key := Key{App: app, DownloadID: row.DownloadID}
			if ev, ok := s.resumedEvent(ctx, row); ok {
				events[key], stamped[key] = ev, row.ActedAt
			}
		}
		if len(events) == 0 {
			continue
		}
		snaps, e, err := s.r.apply(ctx, app, events)
		if err != nil {
			s.r.logger().WarnContext(ctx, "stuck-queue: catch-up write failed", "app", app, "err", err)
			continue
		}
		for _, snap := range snaps {
			if snap.Acted != nil && !snap.Acted.At.Equal(stamped[snap.Key]) {
				caught++
			}
		}
		effs = append(effs, e...)
	}
	return caught, effs
}

func (s *Scheduler) resumedEvent(ctx context.Context, row db.StuckRow) (Event, bool) {
	if row.ActedAction != string(ActionRegrab) || row.ActedResult != string(ResultUnfinished) {
		return Event{}, false
	}
	op, err := s.d.Operation(ctx, row.ActedOpID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		s.r.logger().WarnContext(ctx, "stuck-queue: row names a missing op, left for expiry",
			"app", row.App, "download", row.DownloadID, "op", row.ActedOpID)
		return Event{}, false
	case err != nil:
		s.r.logger().WarnContext(ctx, "stuck-queue: catch-up could not read an op",
			"app", row.App, "download", row.DownloadID, "op", row.ActedOpID, "err", err)
		return Event{}, false
	case !op.UpdatedAt.After(row.ActedAt):
		return Event{}, false
	}
	return Event{Kind: EvResumed, OpID: op.ID, Done: op.Status == operation.Done, At: op.UpdatedAt}, true
}
