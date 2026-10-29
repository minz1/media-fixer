package client

import (
	"context"
	"net/url"
	"strconv"
)

const queryTrue = "true"

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
	HasFile              bool
	HasFileKnown         bool
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
			"includeEpisode":            {queryTrue},
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
		e.HasFile, e.HasFileKnown = *r.EpisodeHasFile, true
	case r.Movie != nil && r.Movie.HasFile != nil:
		e.HasFile, e.HasFileKnown = *r.Movie.HasFile, true
	}
	return e
}
