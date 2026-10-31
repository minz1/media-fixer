package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ArrClient talks to either Sonarr or Radarr via their shared v3 API surface.
type ArrClient struct {
	base   string
	apiKey string
	http   *http.Client
	slow   *http.Client
}

func NewArr(base, apiKey string) *ArrClient {
	return &ArrClient{
		base:   base,
		apiKey: apiKey,
		http:   newHTTPClient(defaultHTTPTimeout),
		slow:   newHTTPClient(slowHTTPTimeout),
	}
}

type Series struct {
	ID         int       `json:"id"`
	Title      string    `json:"title"`
	Path       string    `json:"path"`
	Monitored  bool      `json:"monitored"`
	SeriesType string    `json:"seriesType"`
	Added      time.Time `json:"added"`
}

type Movie struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Path        string `json:"path"`
	MovieFileID int    `json:"movieFileId"`
	HasFile     bool   `json:"hasFile"`
}

type Episode struct {
	ID                    int        `json:"id"`
	SeriesID              int        `json:"seriesId"`
	Title                 string     `json:"title"`
	SeasonNumber          int        `json:"seasonNumber"`
	EpisodeNumber         int        `json:"episodeNumber"`
	EpisodeFileID         int        `json:"episodeFileId"`
	HasFile               bool       `json:"hasFile"`
	Monitored             bool       `json:"monitored"`
	AirDateUtc            *time.Time `json:"airDateUtc"`
	AbsoluteEpisodeNumber int        `json:"absoluteEpisodeNumber"`
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

type HistoryEpisode struct {
	SeasonNumber  int `json:"seasonNumber"`
	EpisodeNumber int `json:"episodeNumber"`
}

// HistoryRecord is one grab/import/failure event from Sonarr/Radarr history.
type HistoryRecord struct {
	ID          int       `json:"id"`
	EpisodeID   int       `json:"episodeId,omitempty"`
	SeriesID    int       `json:"seriesId,omitempty"`
	MovieID     int       `json:"movieId,omitempty"`
	SourceTitle string    `json:"sourceTitle"`
	DownloadID  string    `json:"downloadId"`
	EventType   string    `json:"eventType"`
	Date        time.Time `json:"date"`

	Episode *HistoryEpisode `json:"episode,omitempty"`
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
	if err := c.get(ctx, "/api/v3/series", nil, &series); err != nil {
		return nil, err
	}
	return series, nil
}

// ListMovies returns every movie known to Radarr.
func (c *ArrClient) ListMovies(ctx context.Context) ([]Movie, error) {
	var movies []Movie
	if err := c.get(ctx, "/api/v3/movie", nil, &movies); err != nil {
		return nil, err
	}
	return movies, nil
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

// minFuzzyTitleLen is the shortest normalized title that may be substring-
// matched. Below this, containment stops meaning anything: "up" is a
// substring of "superbad", and every string contains "".
const minFuzzyTitleLen = 4

// findFuzzyTitle returns a pointer to the one item whose normalized title
// contains (or is contained by) want, or nil if there is no match or more
// than one. Used as a fallback after an exact match fails.
//
// Both guards exist because this feeds PlanReplace/ExecuteReplace, which
// delete files: an empty want matched the first item in the library outright
// (strings.Contains(x, "") is always true), and taking the first of several
// candidates silently picked an arbitrary one. Refusing is always recoverable
// — the caller reports ErrNotFound and a human looks — where a wrong match is
// not.
func findFuzzyTitle[T any](items []T, want string, title func(T) string) *T {
	if len(want) < minFuzzyTitleLen {
		return nil
	}
	var match *T
	for i := range items {
		norm := normalizeArrTitle(title(items[i]))
		if len(norm) < minFuzzyTitleLen {
			continue
		}
		if !strings.Contains(norm, want) && !strings.Contains(want, norm) {
			continue
		}
		if match != nil {
			return nil // ambiguous: two different titles both plausibly match
		}
		match = &items[i]
	}
	return match
}

// RescanSeries triggers Sonarr to rescan the disk for a series.
const (
	paramCommandName = "name"
	paramSeriesID    = "seriesId"
)

func (c *ArrClient) RescanSeries(ctx context.Context, seriesID int) error {
	_, err := c.postCommand(ctx, map[string]any{
		paramCommandName: "RescanSeries",
		paramSeriesID:    seriesID,
	})
	return err
}

// RescanMovie triggers Radarr to rescan the disk for a movie.
func (c *ArrClient) RescanMovie(ctx context.Context, movieID int) error {
	_, err := c.postCommand(ctx, map[string]any{
		paramCommandName: "RescanMovie",
		"movieId":        movieID,
	})
	return err
}

// SearchEpisodes triggers Sonarr to search for the given episodes and returns the command id.
func (c *ArrClient) SearchEpisodes(ctx context.Context, episodeIDs []int) (int, error) {
	return c.postCommand(ctx, map[string]any{
		paramCommandName: "EpisodeSearch",
		"episodeIds":     episodeIDs,
	})
}

// SeasonSearch triggers Sonarr to search for every episode in a season.
func (c *ArrClient) SeasonSearch(ctx context.Context, seriesID, season int) error {
	_, err := c.postCommand(ctx, map[string]any{
		paramCommandName: "SeasonSearch",
		paramSeriesID:    seriesID,
		"seasonNumber":   season,
	})
	return err
}

// SeriesSearch triggers Sonarr to search for every monitored episode of a series.
func (c *ArrClient) SeriesSearch(ctx context.Context, seriesID int) error {
	_, err := c.postCommand(ctx, map[string]any{
		paramCommandName: "SeriesSearch",
		paramSeriesID:    seriesID,
	})
	return err
}

// SearchMovieNow triggers Radarr to search for a movie.
func (c *ArrClient) SearchMovieNow(ctx context.Context, movieID int) error {
	_, err := c.postCommand(ctx, map[string]any{
		paramCommandName: "MoviesSearch",
		"movieIds":       []int{movieID},
	})
	return err
}

// GetEpisodes returns episodes for a series, optionally filtered by season.
func (c *ArrClient) GetEpisodes(ctx context.Context, seriesID, season int) ([]Episode, error) {
	q := url.Values{}
	q.Set("seriesId", strconv.Itoa(seriesID))
	if season >= 0 {
		q.Set("seasonNumber", strconv.Itoa(season))
	}

	var episodes []Episode
	if err := c.get(ctx, "/api/v3/episode", q, &episodes); err != nil {
		return nil, err
	}
	return episodes, nil
}

// GetEpisodeFiles returns the on-disk files Sonarr tracks for a series.
func (c *ArrClient) GetEpisodeFiles(ctx context.Context, seriesID int) ([]EpisodeFile, error) {
	q := url.Values{}
	q.Set("seriesId", strconv.Itoa(seriesID))

	var files []EpisodeFile
	if err := c.get(ctx, "/api/v3/episodefile", q, &files); err != nil {
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
	q := url.Values{}
	q.Set("movieId", strconv.Itoa(movieID))

	var files []MovieFile
	if err := c.get(ctx, "/api/v3/moviefile", q, &files); err != nil {
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

const FailedEventType = "downloadFailed"

// SeriesGrabHistory returns grab events for a series, optionally filtered by
// season (pass a negative season to fetch every season).
func (c *ArrClient) SeriesGrabHistory(ctx context.Context, seriesID, season int) ([]HistoryRecord, error) {
	q := seriesHistoryQuery(seriesID, season, grabEventType)

	var records []HistoryRecord
	if err := c.get(ctx, "/api/v3/history/series", q, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func seriesHistoryQuery(seriesID, season int, eventType string) url.Values {
	q := url.Values{}
	q.Set("seriesId", strconv.Itoa(seriesID))
	if eventType != "" {
		q.Set("eventType", eventType)
	}
	if season >= 0 {
		q.Set("seasonNumber", strconv.Itoa(season))
	}
	return q
}

// SeriesEvent is a history record plus the indexer and message from its data;
// the rest of data (download URLs carry indexer API keys) is dropped.
type SeriesEvent struct {
	HistoryRecord

	Indexer string
	Message string
}

var (
	arrAuthHeaderRE = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization)["']? ?[:=] ?["']?(?:[a-z]+ )?[^&\s"']+`)
	arrBearerRE     = regexp.MustCompile(`(?i)\bbearer [^&\s"']+`)
	arrCookieRE     = regexp.MustCompile(
		`(?i)\b((?:set-)?cookie)["']? ?[:=] ?["']?[^&\s"';]+(?:; ?[^&\s"';]+)*["']?`,
	)
	arrCredentialParamRE = regexp.MustCompile(
		`(?i)([a-z_-]*(?:key|token|pass|secret|[_-]auth)[a-z_-]*|\b(?:auth|sig|sid)|[?&]r)["']? ?(?:=|%3D|%253D|:) ?(?:"[^"]*"?|'[^']*'?|[^&\s"]*)`,
	)
	arrURLUserinfoRE = regexp.MustCompile(`://([^/\s:]*):[^/?#\s]+@`)
)

// RedactCredentials collapses whitespace, then replaces every authorization header, bearer token, cookie, key, token,
// pass, secret or auth parameter or header value and URL password in s.
func RedactCredentials(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = arrAuthHeaderRE.ReplaceAllString(s, "${1}: REDACTED")
	s = arrBearerRE.ReplaceAllString(s, "Bearer REDACTED")
	s = arrCookieRE.ReplaceAllString(s, "${1}: REDACTED")
	s = arrCredentialParamRE.ReplaceAllString(s, "${1}=REDACTED")
	return arrURLUserinfoRE.ReplaceAllString(s, "://${1}:REDACTED@")
}

// SeriesHistory returns every history event of a series.
func (c *ArrClient) SeriesHistory(ctx context.Context, seriesID int) ([]SeriesEvent, error) {
	q := seriesHistoryQuery(seriesID, -1, "")
	q.Set("includeEpisode", "true")

	var raw []struct {
		HistoryRecord

		Data struct {
			Indexer string `json:"indexer"`
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v3/history/series", q, &raw); err != nil {
		return nil, err
	}
	events := make([]SeriesEvent, len(raw))
	for i, r := range raw {
		events[i] = SeriesEvent{
			HistoryRecord: r.HistoryRecord,
			Indexer:       r.Data.Indexer,
			Message:       RedactCredentials(r.Data.Message),
		}
	}
	return events, nil
}

// MovieGrabHistory returns grab events for a movie.
func (c *ArrClient) MovieGrabHistory(ctx context.Context, movieID int) ([]HistoryRecord, error) {
	q := url.Values{}
	q.Set("movieId", strconv.Itoa(movieID))
	q.Set("eventType", grabEventType)

	var records []HistoryRecord
	if err := c.get(ctx, "/api/v3/history/movie", q, &records); err != nil {
		return nil, err
	}
	return records, nil
}

// QueueRecord is one in-progress download tracked by Sonarr/Radarr's queue
// (GET /api/v3/queue) — a search having been triggered doesn't mean anything
// is actually downloading yet; the queue is the live signal that it landed
// on a release and is fetching it.
type QueueRecord struct {
	ID        int    `json:"id"`
	SeriesID  int    `json:"seriesId,omitempty"`
	EpisodeID int    `json:"episodeId,omitempty"`
	MovieID   int    `json:"movieId,omitempty"`
	Title     string `json:"title"`
	// Status is Sonarr/Radarr's queue item status, e.g. "queued",
	// "downloading", "completed", "warning", "failed".
	Status   string  `json:"status"`
	Size     float64 `json:"size"`
	SizeLeft float64 `json:"sizeleft"`
}

// queueResponse is the paginated envelope GET /api/v3/queue returns.
type queueResponse struct {
	Records []QueueRecord `json:"records"`
}

// queuePageSize is generously large for a single self-hosted instance's
// queue — large enough that pagination is never needed in practice.
const queuePageSize = 250

// GetQueue returns every item currently in Sonarr/Radarr's download queue —
// the live "is anything actually downloading for this" signal, checked by
// the pending-outcome sweeper instead of guessing how long a search+download
// should take. includeUnknownSeriesItems/includeUnknownMovieItems are
// harmless no-ops on whichever API (Sonarr/Radarr) doesn't recognize them.
func (c *ArrClient) GetQueue(ctx context.Context) ([]QueueRecord, error) {
	q := url.Values{}
	q.Set("pageSize", strconv.Itoa(queuePageSize))
	q.Set("includeUnknownSeriesItems", "true")
	q.Set("includeUnknownMovieItems", "true")
	var resp queueResponse
	if err := c.get(ctx, "/api/v3/queue", q, &resp); err != nil {
		return nil, err
	}
	return resp.Records, nil
}

// MarkHistoryFailed blocklists the release behind a history grab record —
// this, not another search command, is what actually blocklists a release in
// Sonarr/Radarr's v3 API.
func (c *ArrClient) MarkHistoryFailed(ctx context.Context, historyID int) error {
	return c.do(ctx, c.http, http.MethodPost, fmt.Sprintf("/api/v3/history/failed/%d", historyID), nil, nil, nil)
}

func (c *ArrClient) postCommand(ctx context.Context, body any) (int, error) {
	var cmd struct {
		ID int `json:"id"`
	}
	if err := c.do(ctx, c.http, http.MethodPost, "/api/v3/command", nil, body, &cmd); err != nil {
		return 0, err
	}
	return cmd.ID, nil
}

func (c *ArrClient) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, c.http, http.MethodGet, path, q, nil, out)
}

func (c *ArrClient) delete(ctx context.Context, path string) error {
	return c.do(ctx, c.http, http.MethodDelete, path, nil, nil, nil)
}
