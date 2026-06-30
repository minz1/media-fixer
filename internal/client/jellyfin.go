package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type JellyfinClient struct {
	base   string
	apiKey string
	http   *http.Client
}

func NewJellyfin(base, apiKey string) *JellyfinClient {
	return &JellyfinClient{
		base:   base,
		apiKey: apiKey,
		http:   &http.Client{Timeout: defaultHTTPTimeout},
	}
}

type PlaybackInfoResult struct {
	MediaSources []MediaSource `json:"MediaSources"`
	ErrorCode    string        `json:"ErrorCode"`
}

type MediaSource struct {
	ID                   string        `json:"Id"`
	Path                 string        `json:"Path"`
	Protocol             string        `json:"Protocol"`
	SupportsTranscoding  bool          `json:"SupportsTranscoding"`
	SupportsDirectPlay   bool          `json:"SupportsDirectPlay"`
	SupportsDirectStream bool          `json:"SupportsDirectStream"`
	TranscodingURL       string        `json:"TranscodingUrl"`
	Container            string        `json:"Container"`
	Size                 int64         `json:"Size"`
	MediaStreams         []MediaStream `json:"MediaStreams"`
}

type MediaStream struct {
	Type       string `json:"Type"`
	Codec      string `json:"Codec"`
	Width      int    `json:"Width"`
	Height     int    `json:"Height"`
	BitRate    int    `json:"BitRate"`
	IsExternal bool   `json:"IsExternal"`
}

// PlaybackInfo calls the Jellyfin /Items/{id}/PlaybackInfo endpoint and
// returns the media sources. An empty MediaSources slice means Jellyfin
// cannot open the file.
func (c *JellyfinClient) PlaybackInfo(ctx context.Context, itemID string) (*PlaybackInfoResult, error) {
	u := fmt.Sprintf("%s/Items/%s/PlaybackInfo", c.base, url.PathEscape(itemID))

	// POST with a minimal DeviceProfile so Jellyfin returns full info.
	body := strings.NewReader(`{
		"DeviceProfile": {
			"MaxStreamingBitrate": 120000000,
			"DirectPlayProfiles": [{"Type": "Video"}],
			"TranscodingProfiles": [{"Type": "Video", "Container": "ts", "Protocol": "hls"}]
		},
		"UserId": "00000000000000000000000000000000"
	}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin PlaybackInfo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jellyfin PlaybackInfo: status %d", resp.StatusCode)
	}
	var result PlaybackInfoResult
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return nil, fmt.Errorf("jellyfin PlaybackInfo decode: %w", decodeErr)
	}
	return &result, nil
}

type JellyfinItem struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
	Type string `json:"Type"`
	Path string `json:"Path"`
}

type ItemsResponse struct {
	Items            []JellyfinItem `json:"Items"`
	TotalRecordCount int            `json:"TotalRecordCount"`
}

// jellyfinSearchLimit is the maximum number of results returned by SearchItem.
const jellyfinSearchLimit = 5

// SearchItem searches Jellyfin for media items by name, returning up to 5 matches.
// Returns ErrNotFound if no items match.
func (c *JellyfinClient) SearchItem(ctx context.Context, name string) ([]JellyfinItem, error) {
	u, _ := url.Parse(c.base + "/Items")
	q := u.Query()
	q.Set("searchTerm", name)
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Movie,Episode,Series")
	q.Set("Fields", "Path")
	q.Set("Limit", strconv.Itoa(jellyfinSearchLimit))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jellyfin search: status %d", resp.StatusCode)
	}
	var result ItemsResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return nil, decodeErr
	}
	if len(result.Items) == 0 {
		return nil, ErrNotFound
	}
	return result.Items, nil
}

// ListEpisodes returns the episodes Jellyfin has indexed for a series.
// An empty slice means the series exists but has no episodes indexed — the
// classic "Series present, no playable items" failure.
func (c *JellyfinClient) ListEpisodes(ctx context.Context, seriesID string) ([]JellyfinItem, error) {
	u, _ := url.Parse(fmt.Sprintf("%s/Shows/%s/Episodes", c.base, url.PathEscape(seriesID)))
	q := u.Query()
	q.Set("Fields", "Path")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin episodes: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jellyfin episodes: status %d", resp.StatusCode)
	}
	var result ItemsResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return nil, decodeErr
	}
	return result.Items, nil
}

// LibraryScan triggers a full Jellyfin library refresh (POST /Library/Refresh).
// This is the heavier fix for items that exist on disk but are not indexed.
func (c *JellyfinClient) LibraryScan(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/Library/Refresh", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jellyfin library scan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("jellyfin library scan: status %d", resp.StatusCode)
	}
	return nil
}

// ScanStatus reports whether a library scan is currently running and its progress.
type ScanStatus struct {
	Running     bool    `json:"running"`
	ProgressPct float64 `json:"progress_pct"`
}

// scheduledTask is the subset of Jellyfin's /ScheduledTasks entries we read.
type scheduledTask struct {
	Key                       string  `json:"Key"`
	Name                      string  `json:"Name"`
	State                     string  `json:"State"`
	CurrentProgressPercentage float64 `json:"CurrentProgressPercentage"`
}

// ScanStatus queries Jellyfin's scheduled tasks for the library-scan task so the
// agent can avoid re-triggering a scan that is already in progress.
func (c *JellyfinClient) ScanStatus(ctx context.Context) (*ScanStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/ScheduledTasks", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin scan status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("jellyfin scan status: status %d", resp.StatusCode)
	}
	var tasks []scheduledTask
	if decodeErr := json.NewDecoder(resp.Body).Decode(&tasks); decodeErr != nil {
		return nil, decodeErr
	}
	for _, t := range tasks {
		if t.Key == "RefreshLibrary" || strings.Contains(strings.ToLower(t.Name), "scan media library") {
			return &ScanStatus{
				Running:     strings.EqualFold(t.State, "Running"),
				ProgressPct: t.CurrentProgressPercentage,
			}, nil
		}
	}
	return &ScanStatus{Running: false}, nil
}

// DeleteCache removes the metadata and image cache for an item.
func (c *JellyfinClient) DeleteCache(ctx context.Context, itemID string) error {
	paths := []string{
		fmt.Sprintf("/Items/%s/Refresh", url.PathEscape(itemID)),
	}
	for _, p := range paths {
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			c.base+p,
			strings.NewReader(
				`{"Recursive":true,"ImageRefreshMode":"FullRefresh","MetadataRefreshMode":"FullRefresh","ReplaceAllImages":true,"ReplaceAllMetadata":true}`,
			),
		)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Token", c.apiKey)

		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		closeErr := resp.Body.Close()
		if resp.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("jellyfin refresh %s: status %d", p, resp.StatusCode)
		}
		if closeErr != nil {
			return fmt.Errorf("close response body: %w", closeErr)
		}
	}
	return nil
}
