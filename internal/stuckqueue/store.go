package stuckqueue

import (
	"context"
	"errors"
	"slices"

	"github.com/minz1/mediafixer/internal/db"
)

func toRow(s Snapshot) db.StuckRow {
	r := db.StuckRow{
		App: string(s.Key.App), DownloadID: s.Key.DownloadID, State: string(s.State),
		FirstSeen: s.FirstSeen, NotifiedAt: s.NotifiedAt, Misses: s.Misses,
		TargetsFP: s.TargetsFP, Offered: string(s.Offered),
	}
	if s.Import != nil {
		r.ImportCommandID, r.ImportStartedAt = s.Import.CommandID, s.Import.StartedAt
		r.ImportFiles, r.ImportTotal = s.Import.Files.N, s.Import.Files.M
	}
	if a := s.Acted; a != nil {
		r.ActedAction, r.ActedAt, r.ActedResult, r.ActedOpID = string(a.Action), a.At, string(a.Result), a.OpID
		if a.Action == ActionImport {
			r.ImportFiles, r.ImportTotal = a.Files.N, a.Files.M
		}
	}
	return r
}

func fromRow(r db.StuckRow) Snapshot {
	s := Snapshot{
		Key: Key{App: App(r.App), DownloadID: r.DownloadID}, State: State(r.State),
		FirstSeen: r.FirstSeen, NotifiedAt: r.NotifiedAt, Misses: r.Misses,
		TargetsFP: r.TargetsFP, Offered: Offer(r.Offered),
	}
	files := Files{N: r.ImportFiles, M: r.ImportTotal}
	if s.State == StateImportInflight {
		s.Import = &ImportCmd{CommandID: r.ImportCommandID, StartedAt: r.ImportStartedAt, Files: files}
	}
	if r.ActedAction != "" {
		s.Acted = &Acted{
			Action: Action(r.ActedAction), At: r.ActedAt, Result: ActResult(r.ActedResult), OpID: r.ActedOpID,
		}
		if s.Acted.Action == ActionImport {
			s.Acted.Files = files
		}
	}
	return s
}

func (r *Runner) apply(ctx context.Context, app App, events map[Key]Event) ([]Snapshot, []Effect, error) {
	keys := make([]string, 0, len(events))
	for k := range events {
		if k.App == app {
			keys = append(keys, k.DownloadID)
		}
	}
	slices.Sort(keys)
	var snaps []Snapshot
	var effs []Effect
	err := r.DB.ApplyStuck(ctx, string(app), keys, func(cur map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
		var put []db.StuckRow
		var del []string
		snaps, effs, put, del = r.stepRows(ctx, app, cur, keys, events)
		return put, del, nil
	})
	if err != nil {
		return nil, nil, err
	}
	return snaps, effs, nil
}

func (r *Runner) stepRows(
	ctx context.Context, app App, cur map[string]*db.StuckRow, keys []string, events map[Key]Event,
) ([]Snapshot, []Effect, []db.StuckRow, []string) {
	var snaps []Snapshot
	var effs []Effect
	var put []db.StuckRow
	var del []string
	for _, id := range keys {
		k := Key{App: app, DownloadID: id}
		before := Snapshot{Key: k, State: StateUntracked}
		row, stored := cur[id]
		if stored {
			before = fromRow(*row)
		}
		after, e, ok := r.step(ctx, before, stored, events[k])
		if !ok {
			snaps = append(snaps, before)
			continue
		}
		snaps, effs = append(snaps, after), append(effs, e...)
		switch {
		case after.State != StateUntracked:
			put = append(put, toRow(after))
		case stored:
			del = append(del, id)
		}
	}
	return snaps, effs, put, del
}

func (r *Runner) step(ctx context.Context, before Snapshot, stored bool, ev Event) (Snapshot, []Effect, bool) {
	log := r.logger().With("app", before.Key.App, "download", before.Key.DownloadID,
		"state", before.State, "event", ev.Kind)
	if stored {
		if err := Check(before); err != nil {
			log.ErrorContext(ctx, "stuck-queue: stored row is illegal, skipped", "err", err)
			return before, nil, false
		}
	}
	after, effs, err := Next(before, ev)
	if rej := (*RejectError)(nil); errors.As(err, &rej) {
		log.InfoContext(ctx, "stuck-queue: event rejected", "code", rej.Code, "note", rej.Note)
		return before, nil, false
	}
	if err == nil {
		err = Check(after)
	}
	if err != nil {
		log.ErrorContext(ctx, "stuck-queue: step is illegal, skipped", "err", err)
		return before, nil, false
	}
	return after, effs, true
}
