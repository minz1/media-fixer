package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
		http:   &http.Client{Timeout: defaultHTTPTimeout},
	}
}

type Series struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
}

type Movie struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Path        string `json:"path"`
	MovieFileID int    `json:"movieFileId"`
	HasFile     bool   `json:"hasFile"`
}

type Episode struct {
	ID            int    `json:"id"`
	SeriesID      int    `json:"seriesId"`
	Title         string `json:"title"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	EpisodeFileID int    `json:"episodeFileId"`
	HasFile       bool   `json:"hasFile"`
}

// EpisodeFile is a single on-disk file backing an episode, as tracked by Sonarr.
type EpisodeFile struct {
	ID       int    `json:"id"`
	SeriesID int    `json:"seriesId"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
}

// MovieFile is a single on-disk file backing a movie, as tracked by Radarr.
type MovieFile struct {
	ID      int    `json:"id"`
	MovieID int    `json:"movieId"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
}

// HistoryRecord is one grab/import/failure event from Sonarr/Radarr history.
type HistoryRecord struct {
	ID          int    `json:"id"`
	EpisodeID   int    `json:"episodeId,omitempty"`
	SeriesID    int    `json:"seriesId,omitempty"`
	MovieID     int    `json:"movieId,omitempty"`
	SourceTitle string `json:"sourceTitle"`
	DownloadID  string `json:"downloadId"`
	EventType   string `json:"eventType"`
}

// SystemStatus is the subset of Sonarr/Radarr's /system/status we care about
// — mainly as an authenticated reachability probe for live-check tooling.
type SystemStatus struct {
	Version string `json:"version"`
}

// arrYearSuffixRE strips a trailing " (YYYY)" year qualifier some titles carry.
var arrYearSuffixRE = regexp.MustCompile(`\s*\(\d{4}\)\s*$`)

// arrPunctRE strips punctuation so "Marvel's Agents of S.H.I.E.L.D." and
// "Marvels Agents of SHIELD" normalize the same way.
var arrPunctRE = regexp.MustCompile(`[^a-z0-9 ]+`)

// normalizeArrTitle makes a title comparable across the small formatting
// differences between what an LLM supplies and what Sonarr/Radarr store
// (case, punctuation, a leading article, a trailing release year).
func normalizeArrTitle(title string) string {
	// Leading articles stripped so "The Boys" matches a caller-supplied "Boys".
	leadingArticles := []string{"the ", "a ", "an "}

	t := strings.ToLower(strings.TrimSpace(title))
	t = arrYearSuffixRE.ReplaceAllString(t, "")
	t = arrPunctRE.ReplaceAllString(t, "")
	t = strings.TrimSpace(t)
	for _, article := range leadingArticles {
		if trimmed, ok := strings.CutPrefix(t, article); ok {
			t = trimmed
			break
		}
	}
	return strings.Join(strings.Fields(t), " ")
}

// ListSeries returns every series known to Sonarr.
func (c *ArrClient) ListSeries(ctx context.Context) ([]Series, error) {
	var series []Series
	if err := c.get(ctx, "/api/v3/series", &series); err != nil {
		return nil, err
	}
	return series, nil
}

// ListMovies returns every movie known to Radarr.
func (c *ArrClient) ListMovies(ctx context.Context) ([]Movie, error) {
	var movies []Movie
	if err := c.get(ctx, "/api/v3/movie", &movies); err != nil {
		return nil, err
	}
	return movies, nil
}

// SystemStatus calls GET /system/status, a cheap authenticated reachability probe.
func (c *ArrClient) SystemStatus(ctx context.Context) (*SystemStatus, error) {
	var status SystemStatus
	if err := c.get(ctx, "/api/v3/system/status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// SearchSeries finds a series by title, tolerating case, punctuation, a
// leading article, and a trailing release-year qualifier (see
// normalizeArrTitle) — an LLM-supplied title rarely matches Sonarr's stored
// title byte-for-byte. Returns ErrNotFound if no match exists.
func (c *ArrClient) SearchSeries(ctx context.Context, title string) (*Series, error) {
	series, err := c.ListSeries(ctx)
	if err != nil {
		return nil, err
	}
	want := normalizeArrTitle(title)
	if s := findExactTitle(series, want, func(s Series) string { return s.Title }); s != nil {
		return s, nil
	}
	if s := findFuzzyTitle(series, want, func(s Series) string { return s.Title }); s != nil {
		return s, nil
	}
	return nil, ErrNotFound
}

// SearchMovie finds a movie by title using the same normalized matching as
// SearchSeries. Returns ErrNotFound if no match exists.
func (c *ArrClient) SearchMovie(ctx context.Context, title string) (*Movie, error) {
	movies, err := c.ListMovies(ctx)
	if err != nil {
		return nil, err
	}
	want := normalizeArrTitle(title)
	if m := findExactTitle(movies, want, func(m Movie) string { return m.Title }); m != nil {
		return m, nil
	}
	if m := findFuzzyTitle(movies, want, func(m Movie) string { return m.Title }); m != nil {
		return m, nil
	}
	return nil, ErrNotFound
}

// findExactTitle returns a pointer to the first item whose normalized title
// exactly equals want, or nil.
func findExactTitle[T any](items []T, want string, title func(T) string) *T {
	for i := range items {
		if normalizeArrTitle(title(items[i])) == want {
			return &items[i]
		}
	}
	return nil
}

// findFuzzyTitle returns a pointer to the first item whose normalized title
// contains (or is contained by) want, or nil. Used as a fallback after an
// exact match fails.
func findFuzzyTitle[T any](items []T, want string, title func(T) string) *T {
	for i := range items {
		norm := normalizeArrTitle(title(items[i]))
		if strings.Contains(norm, want) || strings.Contains(want, norm) {
			return &items[i]
		}
	}
	return nil
}

// RescanSeries triggers Sonarr to rescan the disk for a series.
const (
	paramCommandName = "name"
	paramSeriesID    = "seriesId"
)

func (c *ArrClient) RescanSeries(ctx context.Context, seriesID int) error {
	return c.postCommand(ctx, map[string]any{
		paramCommandName: "RescanSeries",
		paramSeriesID:    seriesID,
	})
}

// RescanMovie triggers Radarr to rescan the disk for a movie.
func (c *ArrClient) RescanMovie(ctx context.Context, movieID int) error {
	return c.postCommand(ctx, map[string]any{
		paramCommandName: "RescanMovie",
		"movieId":        movieID,
	})
}

// SearchEpisode triggers Sonarr to search for a specific episode.
func (c *ArrClient) SearchEpisode(ctx context.Context, episodeID int) error {
	return c.postCommand(ctx, map[string]any{
		paramCommandName: "EpisodeSearch",
		"episodeIds":     []int{episodeID},
	})
}

// SeasonSearch triggers Sonarr to search for every episode in a season.
func (c *ArrClient) SeasonSearch(ctx context.Context, seriesID, season int) error {
	return c.postCommand(ctx, map[string]any{
		paramCommandName: "SeasonSearch",
		paramSeriesID:    seriesID,
		"seasonNumber":   season,
	})
}

// SeriesSearch triggers Sonarr to search for every monitored episode of a series.
func (c *ArrClient) SeriesSearch(ctx context.Context, seriesID int) error {
	return c.postCommand(ctx, map[string]any{
		paramCommandName: "SeriesSearch",
		paramSeriesID:    seriesID,
	})
}

// SearchMovieNow triggers Radarr to search for a movie.
func (c *ArrClient) SearchMovieNow(ctx context.Context, movieID int) error {
	return c.postCommand(ctx, map[string]any{
		paramCommandName: "MoviesSearch",
		"movieIds":       []int{movieID},
	})
}

// GetEpisodes returns episodes for a series, optionally filtered by season.
func (c *ArrClient) GetEpisodes(ctx context.Context, seriesID, season int) ([]Episode, error) {
	u, _ := url.Parse(c.base + "/api/v3/episode")
	q := u.Query()
	q.Set("seriesId", strconv.Itoa(seriesID))
	if season >= 0 {
		q.Set("seasonNumber", strconv.Itoa(season))
	}
	u.RawQuery = q.Encode()

	var episodes []Episode
	if err := c.get(ctx, u.RequestURI(), &episodes); err != nil {
		return nil, err
	}
	return episodes, nil
}

// GetEpisodeFiles returns the on-disk files Sonarr tracks for a series.
func (c *ArrClient) GetEpisodeFiles(ctx context.Context, seriesID int) ([]EpisodeFile, error) {
	u, _ := url.Parse(c.base + "/api/v3/episodefile")
	q := u.Query()
	q.Set("seriesId", strconv.Itoa(seriesID))
	u.RawQuery = q.Encode()

	var files []EpisodeFile
	if err := c.get(ctx, u.RequestURI(), &files); err != nil {
		return nil, err
	}
	return files, nil
}

// DeleteEpisodeFile deletes a single episode file record (and the underlying
// file, per Sonarr's normal delete-file behavior).
func (c *ArrClient) DeleteEpisodeFile(ctx context.Context, fileID int) error {
	return c.delete(ctx, fmt.Sprintf("/api/v3/episodefile/%d", fileID))
}

// GetMovieFiles returns the on-disk files Radarr tracks for a movie.
func (c *ArrClient) GetMovieFiles(ctx context.Context, movieID int) ([]MovieFile, error) {
	u, _ := url.Parse(c.base + "/api/v3/moviefile")
	q := u.Query()
	q.Set("movieId", strconv.Itoa(movieID))
	u.RawQuery = q.Encode()

	var files []MovieFile
	if err := c.get(ctx, u.RequestURI(), &files); err != nil {
		return nil, err
	}
	return files, nil
}

// DeleteMovieFile deletes a single movie file record (and the underlying
// file, per Radarr's normal delete-file behavior).
func (c *ArrClient) DeleteMovieFile(ctx context.Context, fileID int) error {
	return c.delete(ctx, fmt.Sprintf("/api/v3/moviefile/%d", fileID))
}

// grabEventType is the Sonarr/Radarr history eventType marking a successful
// grab (as opposed to import, failure, or deletion events).
const grabEventType = "grabbed"

// SeriesGrabHistory returns grab events for a series, optionally filtered by
// season (pass a negative season to fetch every season).
func (c *ArrClient) SeriesGrabHistory(ctx context.Context, seriesID, season int) ([]HistoryRecord, error) {
	u, _ := url.Parse(c.base + "/api/v3/history/series")
	q := u.Query()
	q.Set("seriesId", strconv.Itoa(seriesID))
	q.Set("eventType", grabEventType)
	if season >= 0 {
		q.Set("seasonNumber", strconv.Itoa(season))
	}
	u.RawQuery = q.Encode()

	var records []HistoryRecord
	if err := c.get(ctx, u.RequestURI(), &records); err != nil {
		return nil, err
	}
	return records, nil
}

// MovieGrabHistory returns grab events for a movie.
func (c *ArrClient) MovieGrabHistory(ctx context.Context, movieID int) ([]HistoryRecord, error) {
	u, _ := url.Parse(c.base + "/api/v3/history/movie")
	q := u.Query()
	q.Set("movieId", strconv.Itoa(movieID))
	q.Set("eventType", grabEventType)
	u.RawQuery = q.Encode()

	var records []HistoryRecord
	if err := c.get(ctx, u.RequestURI(), &records); err != nil {
		return nil, err
	}
	return records, nil
}

// MarkHistoryFailed blocklists the release behind a history grab record —
// this, not another search command, is what actually blocklists a release in
// Sonarr/Radarr's v3 API.
func (c *ArrClient) MarkHistoryFailed(ctx context.Context, historyID int) error {
	u := fmt.Sprintf("%s/api/v3/history/failed/%d", c.base, historyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("arr POST %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("arr POST %s: status %d", u, resp.StatusCode)
	}
	return nil
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
	if resp.StatusCode >= http.StatusMultipleChoices {
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
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("arr GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *ArrClient) delete(ctx context.Context, path string) error {
	u := c.base + path
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("arr DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("arr DELETE %s: status %d", path, resp.StatusCode)
	}
	return nil
}
