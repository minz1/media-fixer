package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ArrClient talks to either Sonarr or Radarr via their shared v3 API surface.
type ArrClient struct {
	base   string
	apiKey string
	http   *http.Client
}

func NewArr(base, apiKey string) *ArrClient {
	return &ArrClient{
		base:   base,
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

type Series struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
}

type Movie struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
}

type Episode struct {
	ID           int    `json:"id"`
	SeriesID     int    `json:"seriesId"`
	Title        string `json:"title"`
	SeasonNumber int    `json:"seasonNumber"`
	EpisodeNumber int   `json:"episodeNumber"`
}

// SearchSeries finds a series by title.
func (c *ArrClient) SearchSeries(ctx context.Context, title string) (*Series, error) {
	var series []Series
	if err := c.get(ctx, "/api/v3/series", &series); err != nil {
		return nil, err
	}
	for _, s := range series {
		if s.Title == title {
			return &s, nil
		}
	}
	return nil, nil
}

// SearchMovie finds a movie by title.
func (c *ArrClient) SearchMovie(ctx context.Context, title string) (*Movie, error) {
	var movies []Movie
	if err := c.get(ctx, "/api/v3/movie", &movies); err != nil {
		return nil, err
	}
	for _, m := range movies {
		if m.Title == title {
			return &m, nil
		}
	}
	return nil, nil
}

// RescanSeries triggers Sonarr to rescan the disk for a series.
func (c *ArrClient) RescanSeries(ctx context.Context, seriesID int) error {
	return c.postCommand(ctx, map[string]any{
		"name":     "RescanSeries",
		"seriesId": seriesID,
	})
}

// RescanMovie triggers Radarr to rescan the disk for a movie.
func (c *ArrClient) RescanMovie(ctx context.Context, movieID int) error {
	return c.postCommand(ctx, map[string]any{
		"name":    "RescanMovie",
		"movieId": movieID,
	})
}

// SearchEpisode triggers Sonarr to search for a specific episode.
func (c *ArrClient) SearchEpisode(ctx context.Context, episodeID int) error {
	return c.postCommand(ctx, map[string]any{
		"name":       "EpisodeSearch",
		"episodeIds": []int{episodeID},
	})
}

// SearchMovieNow triggers Radarr to search for a movie.
func (c *ArrClient) SearchMovieNow(ctx context.Context, movieID int) error {
	return c.postCommand(ctx, map[string]any{
		"name":    "MoviesSearch",
		"movieIds": []int{movieID},
	})
}

// BlocklistEpisode blocklists the current release for an episode and searches again.
func (c *ArrClient) BlocklistEpisode(ctx context.Context, episodeID int) error {
	return c.postCommand(ctx, map[string]any{
		"name":       "EpisodeSearch",
		"episodeIds": []int{episodeID},
	})
}

// GetEpisodes returns episodes for a series, optionally filtered by season.
func (c *ArrClient) GetEpisodes(ctx context.Context, seriesID, season int) ([]Episode, error) {
	u, _ := url.Parse(c.base + "/api/v3/episode")
	q := u.Query()
	q.Set("seriesId", fmt.Sprintf("%d", seriesID))
	if season >= 0 {
		q.Set("seasonNumber", fmt.Sprintf("%d", season))
	}
	u.RawQuery = q.Encode()

	var episodes []Episode
	if err := c.get(ctx, u.RequestURI(), &episodes); err != nil {
		return nil, err
	}
	return episodes, nil
}

func (c *ArrClient) postCommand(ctx context.Context, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/v3/command", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("arr command: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("arr command: status %d", resp.StatusCode)
	}
	return nil
}

func (c *ArrClient) get(ctx context.Context, path string, out any) error {
	u := c.base + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("arr GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("arr GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
