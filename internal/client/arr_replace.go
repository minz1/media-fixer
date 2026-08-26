package client

import (
	"context"
	"fmt"
)

// Media types and scopes accepted by ReplaceRequest.
//
// A file deleted here under /data/library/{tv,movies} is a SYMLINK into
// /mnt/decypharr/__all__/<torrent>/ — deleting it only removes the symlink
// and Sonarr/Radarr's bookkeeping of it, not the underlying debrid content.
// That is exactly what we want: the stale/bad entry stops being referenced
// and the follow-up search command pulls a fresh release.
const (
	ReplaceMediaTV    = "tv"
	ReplaceMediaMovie = "movie"

	ReplaceScopeEpisode = "episode"
	ReplaceScopeSeason  = "season"
	ReplaceScopeSeries  = "series"
)

// noScope is the season/episode-number sentinel meaning "not applicable".
const noScope = -1

// ReplaceRequest describes a remove-and-re-search action: delete the file(s)
// backing a piece of media, blocklist the grabs that produced them, and
// trigger a fresh search.
type ReplaceRequest struct {
	MediaType string // ReplaceMediaTV | ReplaceMediaMovie
	Title     string
	Scope     string // ReplaceScopeEpisode | ReplaceScopeSeason | ReplaceScopeSeries; ignored for movie
	Season    int    // season number; required when Scope is episode or season
	Episode   int    // episode number; required when Scope is episode
	// SkipBlocklist, if true, still deletes files and searches but does not
	// blocklist the grabs that produced them. Zero value (false) is the safe
	// default: blocklist so the same bad release isn't grabbed again.
	SkipBlocklist bool
}

// ReplaceFile is one file ExecuteReplace will delete.
type ReplaceFile struct {
	ID   int
	Path string
	Size int64
}

// ReplacePlan is the fully-resolved, read-only preview of a ReplaceRequest:
// exactly which files would be deleted, which grabs would be blocklisted,
// and what search would follow. Building a plan makes no changes —
// ExecuteReplace does.
type ReplacePlan struct {
	MediaType     string
	Title         string
	Scope         string
	MediaID       int // seriesID (tv) or movieID (movie)
	SeasonNumber  int // noScope if not applicable
	EpisodeNumber int // noScope if not applicable (human-facing episode number)
	EpisodeID     int // noScope if not applicable (Sonarr's internal episode ID)

	Files            []ReplaceFile
	GrabsToBlocklist []HistoryRecord
	SkipBlocklist    bool
}

// ReplaceResult reports what ExecuteReplace actually did. On a partial
// failure it reflects everything completed before the error occurred.
type ReplaceResult struct {
	DeletedFiles     []ReplaceFile
	BlocklistedGrabs []int
	SearchTriggered  string
}

// PlanReplace resolves a ReplaceRequest against live Sonarr/Radarr state
// without making any changes. Callers should show this plan for approval
// before calling ExecuteReplace.
func (c *ArrClient) PlanReplace(ctx context.Context, req ReplaceRequest) (*ReplacePlan, error) {
	switch req.MediaType {
	case ReplaceMediaMovie:
		return c.planMovieReplace(ctx, req)
	case ReplaceMediaTV:
		return c.planTVReplace(ctx, req)
	default:
		return nil, fmt.Errorf("arr replace: unknown media_type %q", req.MediaType)
	}
}

func (c *ArrClient) planMovieReplace(ctx context.Context, req ReplaceRequest) (*ReplacePlan, error) {
	movie, err := c.SearchMovie(ctx, req.Title)
	if err != nil {
		return nil, err
	}
	files, err := c.GetMovieFiles(ctx, movie.ID)
	if err != nil {
		return nil, err
	}
	grabs, err := c.MovieGrabHistory(ctx, movie.ID)
	if err != nil {
		return nil, err
	}
	plan := &ReplacePlan{
		MediaType:        ReplaceMediaMovie,
		Title:            movie.Title,
		MediaID:          movie.ID,
		SeasonNumber:     noScope,
		EpisodeNumber:    noScope,
		EpisodeID:        noScope,
		GrabsToBlocklist: grabs,
		SkipBlocklist:    req.SkipBlocklist,
	}
	for _, f := range files {
		plan.Files = append(plan.Files, ReplaceFile{ID: f.ID, Path: f.Path, Size: f.Size})
	}
	return plan, nil
}

func (c *ArrClient) planTVReplace(ctx context.Context, req ReplaceRequest) (*ReplacePlan, error) {
	series, err := c.SearchSeries(ctx, req.Title)
	if err != nil {
		return nil, err
	}
	switch req.Scope {
	case ReplaceScopeEpisode:
		return c.planEpisodeReplace(ctx, series, req)
	case ReplaceScopeSeason:
		return c.planSeasonReplace(ctx, series, req)
	case ReplaceScopeSeries:
		return c.planSeriesReplace(ctx, series, req)
	default:
		return nil, fmt.Errorf("arr replace: unknown scope %q for tv", req.Scope)
	}
}

// planEpisodeReplace resolves a single-episode replace: the one file backing
// that episode, and only the grabs that produced that episode specifically.
func (c *ArrClient) planEpisodeReplace(ctx context.Context, series *Series, req ReplaceRequest) (*ReplacePlan, error) {
	episodes, err := c.GetEpisodes(ctx, series.ID, req.Season)
	if err != nil {
		return nil, err
	}
	ep := findEpisodeByNumber(episodes, req.Episode)
	if ep == nil {
		return nil, fmt.Errorf("%w: %s season %d episode %d", ErrNotFound, series.Title, req.Season, req.Episode)
	}

	plan := &ReplacePlan{
		MediaType:     ReplaceMediaTV,
		Title:         series.Title,
		Scope:         ReplaceScopeEpisode,
		MediaID:       series.ID,
		SeasonNumber:  req.Season,
		EpisodeNumber: req.Episode,
		EpisodeID:     ep.ID,
		SkipBlocklist: req.SkipBlocklist,
	}
	if ep.HasFile {
		files, filesErr := c.GetEpisodeFiles(ctx, series.ID)
		if filesErr != nil {
			return nil, filesErr
		}
		for _, f := range files {
			if f.ID == ep.EpisodeFileID {
				plan.Files = append(plan.Files, ReplaceFile{ID: f.ID, Path: f.Path, Size: f.Size})
			}
		}
	}
	grabs, err := c.SeriesGrabHistory(ctx, series.ID, req.Season)
	if err != nil {
		return nil, err
	}
	for _, g := range grabs {
		if g.EpisodeID == ep.ID {
			plan.GrabsToBlocklist = append(plan.GrabsToBlocklist, g)
		}
	}
	return plan, nil
}

// planSeasonReplace resolves every file and grab belonging to one season.
func (c *ArrClient) planSeasonReplace(ctx context.Context, series *Series, req ReplaceRequest) (*ReplacePlan, error) {
	episodes, err := c.GetEpisodes(ctx, series.ID, req.Season)
	if err != nil {
		return nil, err
	}
	seasonFileIDs := make(map[int]bool, len(episodes))
	for _, ep := range episodes {
		if ep.HasFile {
			seasonFileIDs[ep.EpisodeFileID] = true
		}
	}
	files, err := c.GetEpisodeFiles(ctx, series.ID)
	if err != nil {
		return nil, err
	}
	grabs, err := c.SeriesGrabHistory(ctx, series.ID, req.Season)
	if err != nil {
		return nil, err
	}

	plan := &ReplacePlan{
		MediaType:        ReplaceMediaTV,
		Title:            series.Title,
		Scope:            ReplaceScopeSeason,
		MediaID:          series.ID,
		SeasonNumber:     req.Season,
		EpisodeNumber:    noScope,
		EpisodeID:        noScope,
		GrabsToBlocklist: grabs,
		SkipBlocklist:    req.SkipBlocklist,
	}
	for _, f := range files {
		if seasonFileIDs[f.ID] {
			plan.Files = append(plan.Files, ReplaceFile{ID: f.ID, Path: f.Path, Size: f.Size})
		}
	}
	return plan, nil
}

// planSeriesReplace resolves every file and grab across the whole series.
func (c *ArrClient) planSeriesReplace(ctx context.Context, series *Series, req ReplaceRequest) (*ReplacePlan, error) {
	files, err := c.GetEpisodeFiles(ctx, series.ID)
	if err != nil {
		return nil, err
	}
	grabs, err := c.SeriesGrabHistory(ctx, series.ID, noScope)
	if err != nil {
		return nil, err
	}
	plan := &ReplacePlan{
		MediaType:        ReplaceMediaTV,
		Title:            series.Title,
		Scope:            ReplaceScopeSeries,
		MediaID:          series.ID,
		SeasonNumber:     noScope,
		EpisodeNumber:    noScope,
		EpisodeID:        noScope,
		GrabsToBlocklist: grabs,
		SkipBlocklist:    req.SkipBlocklist,
	}
	for _, f := range files {
		plan.Files = append(plan.Files, ReplaceFile{ID: f.ID, Path: f.Path, Size: f.Size})
	}
	return plan, nil
}

// findEpisodeByNumber returns a pointer to the episode with the given
// episode number, or nil.
func findEpisodeByNumber(episodes []Episode, number int) *Episode {
	for i := range episodes {
		if episodes[i].EpisodeNumber == number {
			return &episodes[i]
		}
	}
	return nil
}

// ExecuteReplace applies a previously built ReplacePlan: blocklists grabs,
// deletes files, then triggers the appropriate search command. On error it
// stops and returns everything completed so far so the caller can report
// exactly how far the operation got.
func (c *ArrClient) ExecuteReplace(ctx context.Context, plan *ReplacePlan) (*ReplaceResult, error) {
	result := &ReplaceResult{}

	if !plan.SkipBlocklist {
		for _, g := range plan.GrabsToBlocklist {
			if err := c.MarkHistoryFailed(ctx, g.ID); err != nil {
				return result, fmt.Errorf("arr replace: blocklist grab %d: %w", g.ID, err)
			}
			result.BlocklistedGrabs = append(result.BlocklistedGrabs, g.ID)
		}
	}

	for _, f := range plan.Files {
		if err := c.deleteReplaceFile(ctx, plan.MediaType, f); err != nil {
			return result, fmt.Errorf("arr replace: delete file %d (%s): %w", f.ID, f.Path, err)
		}
		result.DeletedFiles = append(result.DeletedFiles, f)
	}

	cmd, err := c.triggerReplaceSearch(ctx, plan)
	if err != nil {
		return result, err
	}
	result.SearchTriggered = cmd
	return result, nil
}

func (c *ArrClient) deleteReplaceFile(ctx context.Context, mediaType string, f ReplaceFile) error {
	if mediaType == ReplaceMediaMovie {
		return c.DeleteMovieFile(ctx, f.ID)
	}
	return c.DeleteEpisodeFile(ctx, f.ID)
}

// triggerReplaceSearch fires the search command matching the plan's scope
// and returns its name for reporting.
func (c *ArrClient) triggerReplaceSearch(ctx context.Context, plan *ReplacePlan) (string, error) {
	switch {
	case plan.MediaType == ReplaceMediaMovie:
		return "MoviesSearch", c.SearchMovieNow(ctx, plan.MediaID)
	case plan.Scope == ReplaceScopeEpisode:
		return "EpisodeSearch", c.SearchEpisode(ctx, plan.EpisodeID)
	case plan.Scope == ReplaceScopeSeason:
		return "SeasonSearch", c.SeasonSearch(ctx, plan.MediaID, plan.SeasonNumber)
	case plan.Scope == ReplaceScopeSeries:
		return "SeriesSearch", c.SeriesSearch(ctx, plan.MediaID)
	default:
		return "", fmt.Errorf("arr replace: cannot determine search command for media_type=%s scope=%s",
			plan.MediaType, plan.Scope)
	}
}
