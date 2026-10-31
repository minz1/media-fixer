package babysit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/sweep"
)

const (
	maxQualityBytes = 4 << 10
	errInvalidData  = "invalid release data"
	grabNotice      = "Grabbed. Once it finishes downloading, Sonarr imports it; " +
		"if it can't, it appears under Stuck downloads here."
)

var ErrFindRunning = errors.New("a release search for this show is already running")

type Found struct {
	Season   int
	Releases []client.Release
	Missing  []client.Episode
}

type GrabRequest struct {
	Season             int
	GUID, Title        string
	IndexerID          int
	Override           bool
	Quality, Languages json.RawMessage
}

func (s *Service) missingOf(ctx context.Context, seriesID, season int) ([]client.Episode, error) {
	eps, err := s.Sonarr.GetEpisodes(ctx, seriesID, -1)
	if err != nil {
		return nil, err
	}
	entries, err := s.Sonarr.QueueEntries(ctx)
	if err != nil {
		return nil, err
	}
	queued := map[int]bool{}
	for _, d := range entries {
		if d.SeriesID == seriesID {
			queued[d.EpisodeID] = true
		}
	}
	return slices.DeleteFunc(sweep.Missing(eps, queued, s.Now()), func(e client.Episode) bool {
		return e.SeasonNumber != season
	}), nil
}

func (s *Service) FindReleases(ctx context.Context, seriesID, season int) (Found, error) {
	release, ok := claim(&s.mu, &s.finding, seriesID)
	if !ok {
		return Found{}, ErrFindRunning
	}
	defer release()
	rels, err := s.Sonarr.Releases(ctx, seriesID, season)
	if err != nil {
		return Found{}, err
	}
	missing, err := s.missingOf(ctx, seriesID, season)
	if err != nil {
		return Found{}, err
	}
	slices.SortStableFunc(rels, func(a, b client.Release) int {
		return boolRank(multi(b)) - boolRank(multi(a))
	})
	return Found{Season: season, Releases: rels, Missing: missing}, nil
}

func multi(r client.Release) bool { return r.FullSeason || len(r.MappedEpisodeInfo) > 1 }

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func validShape[T map[string]any | []any](raw json.RawMessage) bool {
	var v any
	if len(raw) > maxQualityBytes || json.Unmarshal(raw, &v) != nil {
		return false
	}
	_, ok := v.(T)
	return ok
}

func (s *Service) Grab(ctx context.Context, seriesID int, g GrabRequest) (string, error) {
	var err error
	if g.Override {
		err = s.grabOverride(ctx, seriesID, g)
	} else {
		s.logger().InfoContext(ctx, "monitoring: grab",
			"series", seriesID, "season", g.Season, "title", g.Title, "indexer", g.IndexerID, "override", false)
		err = s.Sonarr.GrabRelease(ctx, g.GUID, g.IndexerID)
	}
	if client.ReleaseExpired(err) {
		return "", errors.New("the search expired; find releases again")
	}
	if err != nil {
		return "", err
	}
	return grabNotice, nil
}

func (s *Service) grabOverride(ctx context.Context, seriesID int, g GrabRequest) error {
	if !validShape[map[string]any](g.Quality) || !validShape[[]any](g.Languages) {
		return errors.New(errInvalidData)
	}
	missing, err := s.missingOf(ctx, seriesID, g.Season)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return fmt.Errorf("nothing missing in season %d to grab it for", g.Season)
	}
	ids := make([]int, len(missing))
	for i, e := range missing {
		ids[i] = e.ID
	}
	s.logger().InfoContext(ctx, "monitoring: grab",
		"series", seriesID, "season", g.Season, "title", g.Title, "indexer", g.IndexerID, "override", true,
		"episodes", ids)
	return s.Sonarr.GrabReleaseOverride(ctx, g.GUID, g.IndexerID, seriesID, ids, g.Quality, g.Languages)
}
