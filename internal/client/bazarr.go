package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	bazarrTimeout           = 30 * time.Second
	bazarrCandidatesTimeout = 120 * time.Second
)

var ErrBazarrItemUnknown = errors.New("bazarr: item not known to bazarr")

type BazarrClient struct {
	base              string
	apiKey            string
	http              *http.Client
	timeout           time.Duration
	candidatesTimeout time.Duration
}

type BazarrTarget struct {
	Movie     bool
	SeriesID  int
	EpisodeID int
	MovieID   int
}

type BazarrSubtitle struct {
	Code2    string `json:"code2"`
	Path     string `json:"path"`
	Forced   bool   `json:"forced"`
	HI       bool   `json:"hi"`
	FileSize int64  `json:"file_size"`
}

type BazarrCandidate struct {
	Provider string  `json:"provider"`
	Release  string  `json:"release_info"`
	Score    float64 `json:"score"`
	Language string  `json:"language"`
}

func NewBazarr(base, apiKey string) *BazarrClient {
	return &BazarrClient{
		base:              strings.TrimRight(base, "/"),
		apiKey:            apiKey,
		http:              newHTTPClient(0),
		timeout:           bazarrTimeout,
		candidatesTimeout: bazarrCandidatesTimeout,
	}
}

func (c *BazarrClient) Configured() bool { return c != nil && c.base != "" }

func (c *BazarrClient) Search(ctx context.Context, t BazarrTarget, lang string, forced, hi bool) error {
	q := url.Values{}
	path := "/api/episodes/subtitles"
	if t.Movie {
		path = "/api/movies/subtitles"
		q.Set("radarrid", strconv.Itoa(t.MovieID))
	} else {
		q.Set("seriesid", strconv.Itoa(t.SeriesID))
		q.Set("episodeid", strconv.Itoa(t.EpisodeID))
	}
	q.Set("language", lang)
	q.Set("forced", strconv.FormatBool(forced))
	q.Set("hi", strconv.FormatBool(hi))
	return c.do(ctx, c.timeout, http.MethodPatch, path, q, http.StatusNoContent, nil)
}

func (c *BazarrClient) Subtitles(ctx context.Context, t BazarrTarget) ([]BazarrSubtitle, error) {
	q := url.Values{}
	path := "/api/episodes"
	if t.Movie {
		path = "/api/movies"
		q.Set("radarrid[]", strconv.Itoa(t.MovieID))
	} else {
		q.Set("episodeid[]", strconv.Itoa(t.EpisodeID))
	}
	var out struct {
		Data []struct {
			Subtitles []BazarrSubtitle `json:"subtitles"`
		} `json:"data"`
	}
	if err := c.do(ctx, c.timeout, http.MethodGet, path, q, http.StatusOK, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, ErrBazarrItemUnknown
	}
	return out.Data[0].Subtitles, nil
}

func (c *BazarrClient) Candidates(ctx context.Context, t BazarrTarget) ([]BazarrCandidate, error) {
	q := url.Values{}
	path := "/api/providers/episodes"
	if t.Movie {
		path = "/api/providers/movies"
		q.Set("radarrid", strconv.Itoa(t.MovieID))
	} else {
		q.Set("episodeid", strconv.Itoa(t.EpisodeID))
	}
	var raw []struct {
		BazarrCandidate

		Release []string `json:"release_info"`
	}
	if err := c.do(ctx, c.candidatesTimeout, http.MethodGet, path, q, http.StatusOK, &raw); err != nil {
		return nil, err
	}
	out := make([]BazarrCandidate, len(raw))
	for i, r := range raw {
		out[i] = r.BazarrCandidate
		out[i].Release = strings.Join(r.Release, ", ")
	}
	return out, nil
}

func (c *BazarrClient) do(
	ctx context.Context, timeout time.Duration, method, path string, q url.Values, want int, out any,
) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("bazarr %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return fmt.Errorf("bazarr %s %s: status %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("bazarr %s %s: %w", method, path, err)
	}
	return nil
}
