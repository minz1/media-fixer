package client

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
)

const (
	queryTrue           = "true"
	queryFalse          = "false"
	paramIncludeEpisode = "includeEpisode"
)

type QueueEntry struct {
	ID                   int
	DownloadID           string
	Title                string
	Status               string
	TrackedDownloadState string
	OutputPath           string
	Messages             []string
	SeriesID             int
	EpisodeID            int
	SeasonNumber         int
	EpisodeNumber        int
	MovieID              int
	File                 FileState
}

type FileState uint8

const (
	FileUnknown FileState = iota
	FileMissing
	FilePresent
)

func fileStateOf(has bool) FileState {
	if has {
		return FilePresent
	}
	return FileMissing
}

type queueEntryJSON struct {
	ID                   int    `json:"id"`
	DownloadID           string `json:"downloadId"`
	Title                string `json:"title"`
	Status               string `json:"status"`
	TrackedDownloadState string `json:"trackedDownloadState"`
	OutputPath           string `json:"outputPath"`
	StatusMessages       []struct {
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
	SeriesID       int   `json:"seriesId"`
	EpisodeID      int   `json:"episodeId"`
	SeasonNumber   int   `json:"seasonNumber"`
	MovieID        int   `json:"movieId"`
	EpisodeHasFile *bool `json:"episodeHasFile"`
	Episode        *struct {
		EpisodeNumber int `json:"episodeNumber"`
	} `json:"episode"`
	Movie *struct {
		HasFile *bool `json:"hasFile"`
	} `json:"movie"`
}

type queueEntriesPage struct {
	TotalRecords int              `json:"totalRecords"`
	Records      []queueEntryJSON `json:"records"`
}

func (c *ArrClient) QueueEntries(ctx context.Context) ([]QueueEntry, error) {
	var out []QueueEntry
	seen := map[int]bool{}
	for page := 1; ; page++ {
		q := url.Values{
			"page":                      {strconv.Itoa(page)},
			"pageSize":                  {strconv.Itoa(queuePageSize)},
			"includeUnknownSeriesItems": {queryTrue},
			"includeUnknownMovieItems":  {queryTrue},
			paramIncludeEpisode:         {queryTrue},
			"includeMovie":              {queryTrue},
		}
		var resp queueEntriesPage
		if err := c.get(ctx, "/api/v3/queue", q, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Records {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r.entry())
		}
		if len(resp.Records) == 0 || len(out) >= resp.TotalRecords || (page-1)*queuePageSize >= resp.TotalRecords {
			return out, nil
		}
	}
}

func (r queueEntryJSON) entry() QueueEntry {
	e := QueueEntry{
		ID: r.ID, DownloadID: r.DownloadID, Title: r.Title, Status: r.Status,
		TrackedDownloadState: r.TrackedDownloadState, OutputPath: r.OutputPath,
		SeriesID: r.SeriesID, EpisodeID: r.EpisodeID, SeasonNumber: r.SeasonNumber, MovieID: r.MovieID,
	}
	for _, m := range r.StatusMessages {
		e.Messages = append(e.Messages, m.Messages...)
	}
	if r.Episode != nil {
		e.EpisodeNumber = r.Episode.EpisodeNumber
	}
	switch {
	case r.EpisodeID != 0 && r.EpisodeHasFile != nil:
		e.File = fileStateOf(*r.EpisodeHasFile)
	case r.Movie != nil && r.Movie.HasFile != nil:
		e.File = fileStateOf(*r.Movie.HasFile)
	}
	return e
}

type CandidateEpisode struct {
	ID            int  `json:"id"`
	SeasonNumber  int  `json:"seasonNumber"`
	EpisodeNumber int  `json:"episodeNumber"`
	HasFile       bool `json:"hasFile"`
}

type ImportCandidate struct {
	Path         string
	Size         int64
	DownloadID   string
	ReleaseGroup string
	ReleaseType  string
	SeriesID     int
	MovieID      int
	SeasonNumber int
	IndexerFlags int
	MovieHasFile bool
	Episodes     []CandidateEpisode
	Quality      json.RawMessage
	Languages    json.RawMessage
	Rejections   []string

	CustomFormatScore int
}

type candidateJSON struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	DownloadID   string `json:"downloadId"`
	ReleaseGroup string `json:"releaseGroup"`
	ReleaseType  string `json:"releaseType"`
	SeriesID     int    `json:"seriesId"`
	Series       *struct {
		ID int `json:"id"`
	} `json:"series"`
	Movie *struct {
		ID      int  `json:"id"`
		HasFile bool `json:"hasFile"`
	} `json:"movie"`
	SeasonNumber      int                `json:"seasonNumber"`
	IndexerFlags      int                `json:"indexerFlags"`
	Episodes          []CandidateEpisode `json:"episodes"`
	Quality           json.RawMessage    `json:"quality"`
	Languages         json.RawMessage    `json:"languages"`
	CustomFormatScore int                `json:"customFormatScore"`
	Rejections        []struct {
		Reason string `json:"reason"`
	} `json:"rejections"`
}

func (c *ArrClient) ManualImportCandidates(ctx context.Context, downloadID string) ([]ImportCandidate, error) {
	return c.manualImportPreview(ctx, url.Values{"downloadId": {downloadID}, "filterExistingFiles": {queryFalse}})
}

func (c *ArrClient) manualImportPreview(ctx context.Context, q url.Values) ([]ImportCandidate, error) {
	var raw []candidateJSON
	if err := c.get(ctx, "/api/v3/manualimport", q, &raw); err != nil {
		return nil, err
	}
	out := make([]ImportCandidate, 0, len(raw))
	for _, r := range raw {
		ic := ImportCandidate{
			Path: r.Path, Size: r.Size, DownloadID: r.DownloadID,
			ReleaseGroup: r.ReleaseGroup, ReleaseType: r.ReleaseType,
			SeriesID: r.SeriesID, SeasonNumber: r.SeasonNumber, IndexerFlags: r.IndexerFlags,
			Episodes: r.Episodes, Quality: r.Quality, Languages: r.Languages, CustomFormatScore: r.CustomFormatScore,
		}
		if r.Series != nil && r.Series.ID != 0 {
			ic.SeriesID = r.Series.ID
		}
		if r.Movie != nil {
			ic.MovieID, ic.MovieHasFile = r.Movie.ID, r.Movie.HasFile
		}
		for _, rj := range r.Rejections {
			ic.Rejections = append(ic.Rejections, rj.Reason)
		}
		out = append(out, ic)
	}
	return out, nil
}

func (c *ArrClient) ManualImport(ctx context.Context, cands []ImportCandidate) (int, error) {
	files := make([]map[string]any, 0, len(cands))
	for _, ic := range cands {
		f := map[string]any{
			"path": ic.Path, "quality": ic.Quality, "languages": ic.Languages, "releaseGroup": ic.ReleaseGroup,
			"indexerFlags": ic.IndexerFlags, "releaseType": ic.ReleaseType, "downloadId": ic.DownloadID,
		}
		if ic.MovieID != 0 {
			f["movieId"] = ic.MovieID
		} else {
			ids := make([]int, 0, len(ic.Episodes))
			for _, e := range ic.Episodes {
				ids = append(ids, e.ID)
			}
			f["seriesId"], f["seasonNumber"], f["episodeIds"] = ic.SeriesID, ic.SeasonNumber, ids
		}
		files = append(files, f)
	}
	return c.postCommand(ctx, map[string]any{paramCommandName: "ManualImport", "importMode": "move", "files": files})
}

func (c *ArrClient) ManualImportPending(ctx context.Context, paths []string) (bool, error) {
	var cmds []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Body   struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		} `json:"body"`
	}
	if err := c.get(ctx, "/api/v3/command", nil, &cmds); err != nil {
		return false, err
	}
	for _, cmd := range cmds {
		if cmd.Name != "ManualImport" || (cmd.Status != "queued" && cmd.Status != "started") {
			continue
		}
		for _, f := range cmd.Body.Files {
			if slices.Contains(paths, f.Path) {
				return true, nil
			}
		}
	}
	return false, nil
}
