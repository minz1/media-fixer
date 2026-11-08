package stuckqueue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

var errSearchFailed = errors.New("removed; search failed")

func ownerOf(a Action) (OwnerAction, bool) {
	switch a {
	case ActionRemove:
		return OwnerRemove, true
	case ActionRegrab:
		return OwnerRegrab, true
	case ActionImport:
		return OwnerImport, true
	}
	return "", false
}

func (s *Scheduler) Act(
	ctx context.Context, app App, downloadID string, action Action, targetsFp string,
) (string, error) {
	release, err := s.claim(holderOwner, s.r.Now())
	if err != nil {
		return "", err
	}
	var u update
	var subject string
	defer func() { s.refreshSubject(subject) }()
	defer func() { release(u) }()
	c := s.r.client(app)
	if c == nil {
		return "", refuse("%s is not configured", app.Name())
	}
	owner, known := ownerOf(action)
	if !known {
		return "", refuse("%s is not offered for this download", action)
	}
	key := Key{App: app, DownloadID: downloadID}
	minAge, err := s.guard(ctx, c, key, owner)
	if err != nil {
		return "", err
	}
	d, cands, err := s.liveChecks(ctx, c, app, downloadID, action, targetsFp)
	if err != nil {
		return "", err
	}
	notice, res, cmdID, err := s.perform(ctx, c, d, action, cands)
	if err != nil && !res.Resumable {
		return "", err
	}
	s.r.logger().InfoContext(ctx, "stuck-queue: owner action",
		"app", d.App, "download", d.DownloadID, "title", d.Title, "action", action, "result", resultWord(res))
	u, subject = s.record(ctx, key, actedEvent(action, res, cmdID, len(cands)), minAge), subjectOf(d)
	return notice, err
}

func (s *Scheduler) liveChecks(
	ctx context.Context, c *client.ArrClient, app App, downloadID string, action Action, targetsFp string,
) (Download, []client.ImportCandidate, error) {
	entries, err := c.QueueEntries(ctx)
	if err != nil {
		return Download{}, nil, err
	}
	downloads := Group(app, entries)
	i := slices.IndexFunc(downloads, func(d Download) bool { return strings.EqualFold(d.DownloadID, downloadID) })
	if i < 0 {
		return Download{}, nil, refuse("this download is no longer in the %s queue", app.Name())
	}
	d := downloads[i]
	if !d.Stuck() {
		return Download{}, nil, refuse("this download is no longer stuck")
	}
	if d.TargetsFP() != targetsFp {
		return Download{}, nil, refuse(changedRefusal)
	}
	v := Classify(d)
	var cands []client.ImportCandidate
	var guardErr error
	if v == VerdictGap && !d.MaybeHasFile() {
		cands, guardErr = c.ManualImportCandidates(ctx, d.DownloadID)
		if guardErr != nil {
			guardErr = refuse("couldn't read the import candidates: %v", guardErr)
		} else {
			guardErr = ImportGuard(d, cands, entries)
		}
	}
	if !slices.Contains(offered(d, v, guardErr), action) {
		if action == ActionImport && guardErr != nil {
			return Download{}, nil, guardErr
		}
		return Download{}, nil, refuse("%s is not offered for this download", action)
	}
	if action == ActionImport {
		if err = s.refusePending(ctx, c, cands); err != nil {
			return Download{}, nil, err
		}
	}
	return d, cands, nil
}

func actedEvent(action Action, res RegrabResult, cmdID, files int) Event {
	ev := Event{Kind: EvActed, Action: action}
	switch action {
	case ActionRegrab:
		ev.OpID, ev.Resumable = res.Op.ID, res.Resumable
	case ActionImport:
		ev.Files = Files{N: files, M: files}
		if cmdID > 0 {
			ev.Kind, ev.CommandID = EvImportPosted, cmdID
		}
	case ActionRemove:
	}
	return ev
}

func resultWord(res RegrabResult) ActResult {
	if res.Resumable {
		return ResultUnfinished
	}
	return ResultOK
}

func (s *Scheduler) refusePending(ctx context.Context, c *client.ArrClient, cands []client.ImportCandidate) error {
	paths := make([]string, 0, len(cands))
	for _, ic := range cands {
		paths = append(paths, ic.Path)
	}
	pending, err := c.ManualImportPending(ctx, paths)
	if err != nil {
		return err
	}
	if pending {
		return refuse("an import for this download is already running")
	}
	return nil
}

func (s *Scheduler) guard(ctx context.Context, c *client.ArrClient, key Key, a OwnerAction) (time.Duration, error) {
	set, err := LoadSettings(ctx, s.d)
	if err != nil {
		return 0, err
	}
	row, stored, err := s.d.GetStuck(ctx, string(key.App), key.DownloadID)
	if err != nil {
		return 0, err
	}
	snap := Snapshot{Key: key, State: StateUntracked}
	if stored {
		snap = fromRow(row)
	}
	now := s.r.Now()
	if snap.State == StateImportInflight {
		if snap, err = s.observeInflight(ctx, c, row, set.ImportTimeout(), now); err != nil {
			return 0, err
		}
	}
	return set.MinAge(), Allowed(snap, a, now, set.MinAge())
}

func (s *Scheduler) observeInflight(
	ctx context.Context, c *client.ArrClient, row db.StuckRow, timeout time.Duration, now time.Time,
) (Snapshot, error) {
	cmd, ok := s.readCommand(ctx, c, row)
	if !ok {
		return Snapshot{}, ctx.Err()
	}
	key := Key{App: App(row.App), DownloadID: row.DownloadID}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opSettleBound)
	defer cancel()
	snaps, effs, err := s.r.apply(wctx, key.App, map[Key]Event{key: {
		Kind: EvCommandObserved, Cmd: cmd, Timeout: timeout, At: now,
	}})
	if err != nil {
		return Snapshot{}, err
	}
	s.runEffects(effs)
	return snaps[0], nil
}

func (s *Scheduler) runEffects(effs []Effect) {
	for _, e := range effs {
		if r, ok := e.(EffRefreshCard); ok {
			s.refresh(subjectOf(Download{App: r.Key.App, DownloadID: r.Key.DownloadID}))
		}
	}
}

func (s *Scheduler) record(ctx context.Context, key Key, ev Event, minAge time.Duration) update {
	ev.At, ev.MinAge = s.r.Now(), minAge
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opSettleBound)
	defer cancel()
	snaps, _, err := s.r.apply(wctx, key.App, map[Key]Event{key: ev})
	if err != nil || len(snaps) == 0 {
		s.r.logger().ErrorContext(ctx, "stuck-queue: recording the owner action failed",
			"app", key.App, "download", key.DownloadID, "event", ev.Kind, "err", err)
		return nil
	}
	return updateLast(snaps[0], Actions(snaps[0], ev.At, minAge))
}

func (s *Scheduler) ImportMapped(
	ctx context.Context, seriesID int, downloadID, targetsFp string, rows []MappedFile,
) (string, error) {
	if s.base.Err() != nil {
		return "", card.ErrRestarting
	}
	if len(rows) == 0 {
		return "", refuse("Tick at least one file.")
	}
	release, err := s.claim(holderOwner, s.r.Now())
	if err != nil {
		return "", err
	}
	var u update
	var subject string
	defer func() { s.refreshSubject(subject) }()
	defer func() { release(u) }()
	c := s.r.client(AppSonarr)
	if c == nil {
		return "", refuse("Sonarr is not configured")
	}
	key := Key{App: AppSonarr, DownloadID: downloadID}
	minAge, err := s.guard(ctx, c, key, OwnerMappedImport)
	if err != nil {
		return "", err
	}
	entries, err := c.QueueEntries(ctx)
	if err != nil {
		return "", err
	}
	d, err := ownedStuck(entries, seriesID, downloadID, targetsFp)
	if err != nil {
		return "", err
	}
	cands, err := c.ManualImportCandidates(ctx, d.DownloadID)
	if err != nil {
		return "", err
	}
	eps, err := c.GetEpisodes(ctx, seriesID, -1)
	if err != nil {
		return "", err
	}
	if err = s.refusePending(ctx, c, cands); err != nil {
		return "", err
	}
	results, err := CheckMapping(seriesID, rows, cands, eps, entries, d.DownloadID)
	if err != nil {
		return "", err
	}
	files, labels := mappedCandidates(seriesID, results, cands, eps)
	cmdID, err := c.ManualImport(ctx, files)
	if err != nil {
		return "", err
	}
	s.r.logger().InfoContext(ctx, "stuck-queue: manual import",
		"series", seriesID, "download", d.DownloadID, "files", labels.lines)
	ev := Event{Kind: EvActed, Action: ActionImport, Files: Files{N: len(files), M: len(cands)}}
	if cmdID > 0 {
		ev.Kind, ev.CommandID, ev.Mapped = EvImportPosted, cmdID, true
	}
	u, subject = s.record(ctx, key, ev, minAge), subjectOf(d)
	return fmt.Sprintf(
		"Import started for %d files (%s). Sonarr moves them; the page shows them on disk once it is done.",
		len(files),
		labels.span(),
	), nil
}

func ownedStuck(entries []client.QueueEntry, seriesID int, downloadID, targetsFp string) (Download, error) {
	downloads := Group(AppSonarr, entries)
	i := slices.IndexFunc(downloads, func(d Download) bool { return strings.EqualFold(d.DownloadID, downloadID) })
	if i < 0 {
		return Download{}, refuse("this download is no longer in the Sonarr queue")
	}
	d := downloads[i]
	switch {
	case !d.Stuck():
		return Download{}, refuse("this download is no longer stuck")
	case !d.OwnedBy(seriesID):
		return Download{}, refuse("this download is not only this show's")
	case d.TargetsFP() != targetsFp:
		return Download{}, refuse("this download changed; propose again")
	}
	return d, nil
}

type importLabels struct {
	lines []string
	codes []episodeCode
}

type episodeCode struct{ season, episode int }

func (e episodeCode) String() string { return fmt.Sprintf("S%02dE%02d", e.season, e.episode) }

func (l importLabels) span() string {
	lo, hi := slices.MinFunc(l.codes, compareCodes), slices.MaxFunc(l.codes, compareCodes)
	if lo == hi {
		return lo.String()
	}
	return lo.String() + "–" + hi.String()
}

func compareCodes(a, b episodeCode) int {
	return cmp.Or(cmp.Compare(a.season, b.season), cmp.Compare(a.episode, b.episode))
}

func mappedCandidates(
	seriesID int, results []RowResult, cands []client.ImportCandidate, eps []client.Episode,
) ([]client.ImportCandidate, importLabels) {
	byPath := map[string]client.ImportCandidate{}
	for _, ic := range cands {
		byPath[ic.Path] = ic
	}
	byID := map[int]client.Episode{}
	for _, e := range eps {
		byID[e.ID] = e
	}
	var labels importLabels
	files := make([]client.ImportCandidate, 0, len(results))
	for _, r := range results {
		ic := byPath[r.File.Path]
		ic.SeriesID, ic.SeasonNumber = seriesID, r.Season
		ic.Episodes = nil
		var line []string
		for _, id := range r.File.EpisodeIDs {
			e := byID[id]
			ic.Episodes = append(ic.Episodes, client.CandidateEpisode{
				ID: id, SeasonNumber: e.SeasonNumber, EpisodeNumber: e.EpisodeNumber,
			})
			code := episodeCode{e.SeasonNumber, e.EpisodeNumber}
			labels.codes = append(labels.codes, code)
			line = append(line, code.String())
		}
		labels.lines = append(labels.lines, r.File.Path+" → "+strings.Join(line, " "))
		files = append(files, ic)
	}
	return files, labels
}

func (s *Scheduler) perform(
	ctx context.Context, c *client.ArrClient, d Download, action Action, cands []client.ImportCandidate,
) (string, RegrabResult, int, error) {
	var none RegrabResult
	switch action {
	case ActionImport:
		id, err := c.ManualImport(ctx, cands)
		if err != nil {
			return "", none, 0, err
		}
		return "Import started for " + d.Targets() + ".", none, id, nil
	case ActionRegrab:
		notice, res, err := s.regrab(ctx, c, d)
		return notice, res, 0, err
	case ActionRemove:
		if err := c.RemoveQueueItem(ctx, d.Records[0].ID, false, false, true); err != nil {
			return "", none, 0, err
		}
		return "Removed.", none, 0, nil
	}
	return "", none, 0, refuse("unknown action %q", action)
}

func updateLast(snap Snapshot, actions []Action) update {
	return func(sl *slot) {
		apps := slices.Clone(sl.last.Apps)
		for i := range apps {
			if apps[i].App == snap.Key.App {
				apps[i].Items = withSnapshot(apps[i].Items, snap, actions)
				apps[i].Waiting = withSnapshot(apps[i].Waiting, snap, actions)
			}
		}
		sl.last.Apps = apps
	}
}

func withSnapshot(items []Item, snap Snapshot, actions []Action) []Item {
	out := slices.Clone(items)
	for i := range out {
		if strings.EqualFold(out[i].DownloadID, snap.Key.DownloadID) {
			out[i].State, out[i].Acted, out[i].Actions = snap.State, snap.Acted, actions
		}
	}
	return out
}

func (s *Scheduler) refreshSubject(subject string) {
	if subject != "" {
		s.refresh(subject)
	}
}

func (s *Scheduler) regrab(ctx context.Context, c *client.ArrClient, d Download) (string, RegrabResult, error) {
	res, err := s.runRegrab(ctx, c, d)
	if res.Op.ID != 0 {
		s.trackOp(res.Op)
	}
	if err != nil {
		return "", res, err
	}
	return "Re-grab started for " + d.Targets() + ".", res, nil
}
