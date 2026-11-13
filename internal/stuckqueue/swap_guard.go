package stuckqueue

import (
	"path/filepath"
	"strings"

	"github.com/minz1/mediafixer/internal/client"
)

func SwapGuard(
	series, episodeID, season, brokenFileID int,
	brokenPath, root string,
	cand client.ImportCandidate,
	episodes []client.Episode,
	queue []client.QueueEntry,
) error {
	name := filepath.Base(cand.Path)
	// shortcut: one episode only, a file covering several is refused; upgrade when the broken file covers all of them.
	switch {
	case cand.SeriesID != series:
		return refuse("%s belongs to another series", name)
	case len(cand.Episodes) != 1:
		return refuse("%s must map to exactly one episode", name)
	case cand.Episodes[0].ID != episodeID:
		return refuse("%s maps to an episode that is not the incident's episode", name)
	case cand.SeasonNumber != season || cand.Episodes[0].SeasonNumber != season:
		return refuse("%s is not in season %d", name, season)
	}
	if err := checkBroken(series, episodeID, season, brokenFileID, episodes); err != nil {
		return err
	}
	for _, q := range queue {
		if q.EpisodeID == episodeID && (cand.DownloadID == "" || !strings.EqualFold(q.DownloadID, cand.DownloadID)) {
			return refuse("another download is queued for this episode")
		}
	}
	clean := filepath.Clean(cand.Path)
	if clean == filepath.Clean(brokenPath) {
		return refuse("the candidate is the broken file")
	}
	if !strings.HasPrefix(clean, filepath.Clean(root)+string(filepath.Separator)) {
		return refuse("%s is not under %s", cand.Path, root)
	}
	return nil
}

func checkBroken(series, episodeID, season, brokenFileID int, episodes []client.Episode) error {
	var current client.Episode
	covered := 0
	for _, e := range episodes {
		if e.SeriesID != series {
			continue
		}
		if e.ID == episodeID {
			current = e
		}
		if brokenFileID > 0 && e.EpisodeFileID == brokenFileID {
			covered++
		}
	}
	switch {
	case current.ID == 0:
		return refuse("episode %d is not in this show", episodeID)
	case current.SeasonNumber != season:
		return refuse("episode %d is not in season %d", episodeID, season)
	case brokenFileID == 0 || current.EpisodeFileID != brokenFileID:
		return refuse("the broken file is no longer the episode's file")
	case covered > 1:
		return refuse("the broken file covers more than one episode")
	}
	return nil
}
