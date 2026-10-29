package stuckqueue

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/minz1/mediafixer/internal/client"
)

func (s *Scheduler) Act(
	ctx context.Context, app App, downloadID string, action Action, seenTargets string,
) (string, error) {
	if !s.mu.TryLock() {
		return "", refuse("a run is active, try again in a moment")
	}
	defer s.mu.Unlock()
	c := s.r.client(app)
	if c == nil {
		return "", refuse("%s is not configured", appName(app))
	}
	entries, err := c.QueueEntries(ctx)
	if err != nil {
		return "", err
	}
	downloads := Group(app, entries)
	i := slices.IndexFunc(downloads, func(d Download) bool { return strings.EqualFold(d.DownloadID, downloadID) })
	if i < 0 {
		return "", refuse("this download is no longer in the %s queue", appName(app))
	}
	d := downloads[i]
	if !d.Stuck() {
		return "", refuse("this download is no longer stuck")
	}
	if now := d.Targets(); now != seenTargets {
		return "", refuse("this download now maps to %s, not %s; check the card again", now, seenTargets)
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
	if err != nil {
		return "", err
	}
	s.r.logger().InfoContext(ctx, "stuck-queue: owner action",
		"app", app, "download", d.DownloadID, "title", d.Title, "action", action)
	s.forget(app, d.DownloadID)
	return notice, nil
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
			return "", fmt.Errorf("the release is blocklisted and removed, but the search failed; search from %s: %w",
				appName(d.App), err)
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
	for i := range apps {
		if apps[i].App == app {
			apps[i].Items = slices.DeleteFunc(slices.Clone(apps[i].Items), func(it Item) bool {
				return strings.EqualFold(it.DownloadID, downloadID)
			})
		}
	}
	s.last.Apps = apps
}
