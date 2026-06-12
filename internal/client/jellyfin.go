package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
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
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

type PlaybackInfoResult struct {
	MediaSources     []MediaSource `json:"MediaSources"`
	ErrorCode        string        `json:"ErrorCode"`
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
	MediaStreams          []MediaStream `json:"MediaStreams"`
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
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jellyfin PlaybackInfo: status %d", resp.StatusCode)
	}
	var result PlaybackInfoResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("jellyfin PlaybackInfo decode: %w", err)
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

// SearchItem searches Jellyfin for a media item by name, returning the first match.
func (c *JellyfinClient) SearchItem(ctx context.Context, name string) (*JellyfinItem, error) {
	u, _ := url.Parse(c.base + "/Items")
	q := u.Query()
	q.Set("searchTerm", name)
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Movie,Episode,Series")
	q.Set("Fields", "Path")
	q.Set("Limit", "1")
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
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jellyfin search: status %d", resp.StatusCode)
	}
	var result ItemsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, nil
	}
	item := result.Items[0]
	return &item, nil
}

// DeleteCache removes the metadata and image cache for an item.
func (c *JellyfinClient) DeleteCache(ctx context.Context, itemID string) error {
	paths := []string{
		fmt.Sprintf("/Items/%s/Refresh", url.PathEscape(itemID)),
	}
	for _, p := range paths {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+p, strings.NewReader(`{"Recursive":true,"ImageRefreshMode":"FullRefresh","MetadataRefreshMode":"FullRefresh","ReplaceAllImages":true,"ReplaceAllMetadata":true}`))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Token", c.apiKey)
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
	}
	return nil
}
