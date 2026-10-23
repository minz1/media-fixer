package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	slowHTTPTimeout  = 15 * time.Minute
	statusBodyLimit  = 200
	pathRelease      = "/api/v3/release"
	paramSeasonQuery = "seasonNumber"
)

type Release struct {
	GUID          string   `json:"guid"`
	IndexerID     int      `json:"indexerId"`
	Title         string   `json:"title"`
	Indexer       string   `json:"indexer"`
	Protocol      string   `json:"protocol"`
	FullSeason    bool     `json:"fullSeason"`
	Rejections    []string `json:"rejections"`
	ReleaseWeight int      `json:"releaseWeight"`
	SeasonNumber  int      `json:"seasonNumber"`
	Size          int64    `json:"size"`
	Quality       struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	} `json:"quality"`
	MappedEpisodeInfo []struct {
		ID int `json:"id"`
	} `json:"mappedEpisodeInfo"`
}

type QueueDetail struct {
	ID                   int    `json:"id"`
	DownloadID           string `json:"downloadId"`
	SeriesID             int    `json:"seriesId"`
	EpisodeID            int    `json:"episodeId"`
	SeasonNumber         int    `json:"seasonNumber"`
	Title                string `json:"title"`
	TrackedDownloadState string `json:"trackedDownloadState"`
	StatusMessages       []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
}

type DownloadClientConfig struct {
	AutoRedownloadFailed                      bool `json:"autoRedownloadFailed"`
	AutoRedownloadFailedFromInteractiveSearch bool `json:"autoRedownloadFailedFromInteractiveSearch"`
}

type HTTPStatusError struct {
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.Status, e.Body)
}

func (e *HTTPStatusError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

func (c *ArrClient) SetSlowTimeout(d time.Duration) {
	c.slow.Timeout = d
}

func (c *ArrClient) Releases(ctx context.Context, seriesID, season int) ([]Release, error) {
	q := url.Values{paramSeriesID: {strconv.Itoa(seriesID)}, paramSeasonQuery: {strconv.Itoa(season)}}
	var rels []Release
	err := c.do(ctx, c.slow, http.MethodGet, pathRelease, q, nil, &rels)
	return rels, err
}

func (c *ArrClient) EpisodeReleases(ctx context.Context, episodeID int) ([]Release, error) {
	q := url.Values{"episodeId": {strconv.Itoa(episodeID)}}
	var rels []Release
	err := c.do(ctx, c.slow, http.MethodGet, pathRelease, q, nil, &rels)
	return rels, err
}

func (c *ArrClient) GrabRelease(ctx context.Context, guid string, indexerID int) error {
	body := map[string]any{"guid": guid, "indexerId": indexerID}
	return c.do(ctx, c.slow, http.MethodPost, pathRelease, nil, body, nil)
}

func (c *ArrClient) HistorySince(ctx context.Context, since time.Time) ([]HistoryRecord, error) {
	q := url.Values{"date": {since.UTC().Format(time.RFC3339)}}
	var recs []HistoryRecord
	err := c.do(ctx, c.http, http.MethodGet, "/api/v3/history/since", q, nil, &recs)
	return recs, err
}

func (c *ArrClient) QueueDetails(ctx context.Context) ([]QueueDetail, error) {
	q := url.Values{"includeEpisode": {"true"}}
	var recs []QueueDetail
	err := c.do(ctx, c.http, http.MethodGet, "/api/v3/queue/details", q, nil, &recs)
	return recs, err
}

func (c *ArrClient) RemoveQueueItem(
	ctx context.Context,
	id int,
	removeFromClient, blocklist, skipRedownload bool,
) error {
	q := url.Values{
		"removeFromClient": {strconv.FormatBool(removeFromClient)},
		"blocklist":        {strconv.FormatBool(blocklist)},
		"skipRedownload":   {strconv.FormatBool(skipRedownload)},
	}
	return c.do(ctx, c.http, http.MethodDelete, fmt.Sprintf("/api/v3/queue/%d", id), q, nil, nil)
}

func (c *ArrClient) Command(ctx context.Context, id int) (string, error) {
	var cmd struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, c.http, http.MethodGet, fmt.Sprintf("/api/v3/command/%d", id), nil, nil, &cmd)
	return cmd.Status, err
}

func (c *ArrClient) DownloadClientConfig(ctx context.Context) (DownloadClientConfig, error) {
	var cfg DownloadClientConfig
	err := c.do(ctx, c.http, http.MethodGet, "/api/v3/config/downloadclient", nil, nil, &cfg)
	return cfg, err
}

func (c *ArrClient) do(ctx context.Context, hc *http.Client, method, path string, q url.Values, body, out any) error {
	u := c.base + path
	if q != nil {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("arr %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, statusBodyLimit))
		return fmt.Errorf("arr %s %s: %w", method, path, &HTTPStatusError{Status: resp.StatusCode, Body: string(b)})
	}
	if out == nil {
		return nil
	}
	if derr := json.NewDecoder(resp.Body).Decode(out); derr != nil {
		return fmt.Errorf("arr %s %s: %w", method, path, derr)
	}
	return nil
}
