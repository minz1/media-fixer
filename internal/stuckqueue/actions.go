package stuckqueue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/minz1/mediafixer/internal/client"
)

var errSearchFailed = errors.New("removed; search failed")

func (s *Scheduler) Act(
	ctx context.Context, app App, downloadID string, action Action, targetsFp string,
) (string, error) {
	if !s.mu.TryLock() {
		return "", refuse("a run is active, try again in a moment")
	}
	defer s.mu.Unlock()
	c := s.r.client(app)
	if c == nil {
		return "", refuse("%s is not configured", app.Name())
	}
	entries, err := c.QueueEntries(ctx)
	if err != nil {
		return "", err
	}
	downloads := Group(app, entries)
	i := slices.IndexFunc(downloads, func(d Download) bool { return strings.EqualFold(d.DownloadID, downloadID) })
	if i < 0 {
		return "", refuse("this download is no longer in the %s queue", app.Name())
	}
	d := downloads[i]
	if !d.Stuck() {
		return "", refuse("this download is no longer stuck")
	}
	if d.TargetsFP() != targetsFp {
		return "", refuse("this download now maps to %s; check the card again", d.Targets())
	}
	if action != ActionRemove {
		if err = s.waitOver(ctx, app, d.DownloadID); err != nil {
			return "", err
		}
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
			return "", guardErr
		}
		return "", refuse("%s is not offered for this download", action)
	}
	notice, err := s.apply(ctx, c, d, action, cands)
	searchFailed := errors.Is(err, errSearchFailed)
	if err != nil && !searchFailed {
		return "", err
	}
	s.r.logger().InfoContext(ctx, "stuck-queue: owner action",
		"app", app, "download", d.DownloadID, "title", d.Title, "action", action, "search_failed", searchFailed)
	s.recordOutcome(d, action, searchFailed)
	s.forget(app, d.DownloadID)
	s.refresh(subjectOf(d))
	return notice, err
}

func (s *Scheduler) ImportMapped(
	ctx context.Context, seriesID int, downloadID, targetsFp string, rows []MappedFile,
) (string, error) {
	if len(rows) == 0 {
		return "", refuse("Tick at least one file.")
	}
	if !s.mu.TryLock() {
		return "", refuse("a run is active, try again in a moment")
	}
	defer s.mu.Unlock()
	c := s.r.client(AppSonarr)
	if c == nil {
		return "", refuse("Sonarr is not configured")
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
	paths := make([]string, 0, len(cands))
	for _, ic := range cands {
		paths = append(paths, ic.Path)
	}
	pending, err := c.ManualImportPending(ctx, paths)
	if err != nil {
		return "", err
	}
	if pending {
		return "", refuse("an import for this download is already running")
	}
	results, err := CheckMapping(seriesID, rows, cands, eps, entries, d.DownloadID)
	if err != nil {
		return "", err
	}
	files, labels := mappedCandidates(seriesID, results, cands, eps)
	if _, err = c.ManualImport(ctx, files); err != nil {
		return "", err
	}
	s.r.logger().InfoContext(ctx, "stuck-queue: manual import",
		"series", seriesID, "download", d.DownloadID, "files", labels.lines)
	if len(files) == len(cands) {
		s.recordOutcome(d, ActionImport, false)
		s.forget(AppSonarr, d.DownloadID)
	}
	s.refresh(subjectOf(d))
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

func (s *Scheduler) waitOver(ctx context.Context, app App, downloadID string) error {
	set, err := LoadSettings(ctx, s.d)
	if err != nil {
		return err
	}
	row, ok, err := s.d.GetStuckSeen(ctx, string(app), downloadID)
	if err != nil {
		return err
	}
	if !ok {
		return refuse("no run has checked this download yet; only Remove is allowed until one does")
	}
	if due := row.FirstSeenAt.Add(set.MinAge()); s.r.Now().Before(due) {
		return refuse("this download is inside the %d h wait until %s; only Remove is allowed",
			set.MinAgeHours, due.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return nil
}

func (s *Scheduler) apply(
	ctx context.Context, c *client.ArrClient, d Download, action Action, cands []client.ImportCandidate,
) (string, error) {
	switch action {
	case ActionImport:
		if _, err := c.ManualImport(ctx, cands); err != nil {
			return "", err
		}
		return "Import started for " + d.Targets() + ".", nil
	case ActionRegrab:
		if err := c.RemoveQueueItem(ctx, d.Records[0].ID, false, true, true); err != nil {
			return "", err
		}
		if err := search(ctx, c, d); err != nil {
			return "", fmt.Errorf("%w: %w", errSearchFailed, err)
		}
		return "Re-grab started for " + d.Targets() + ".", nil
	case ActionRemove:
		if err := c.RemoveQueueItem(ctx, d.Records[0].ID, false, false, true); err != nil {
			return "", err
		}
		return "Removed.", nil
	}
	return "", refuse("unknown action %q", action)
}

func search(ctx context.Context, c *client.ArrClient, d Download) error {
	if d.App == AppRadarr {
		return c.SearchMovieNow(ctx, d.Records[0].MovieID)
	}
	ids := make([]int, 0, len(d.Records))
	for _, r := range d.Records {
		ids = append(ids, r.EpisodeID)
	}
	_, err := c.SearchEpisodes(ctx, ids)
	return err
}

func (s *Scheduler) forget(app App, downloadID string) {
	s.smu.Lock()
	defer s.smu.Unlock()
	apps := slices.Clone(s.last.Apps)
	gone := func(it Item) bool { return strings.EqualFold(it.DownloadID, downloadID) }
	for i := range apps {
		if apps[i].App == app {
			apps[i].Items = slices.DeleteFunc(slices.Clone(apps[i].Items), gone)
			apps[i].Waiting = slices.DeleteFunc(slices.Clone(apps[i].Waiting), gone)
		}
	}
	s.last.Apps = apps
}
