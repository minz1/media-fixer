package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type DecypharrClient struct {
	base     string
	apiToken string
	http     *http.Client
}

func NewDecypharr(base, apiToken string) *DecypharrClient {
	return &DecypharrClient{
		base:     base,
		apiToken: apiToken,
		http:     &http.Client{Timeout: defaultHTTPTimeout},
	}
}

type TorrentEntry struct {
	Name     string    `json:"name"`
	InfoHash string    `json:"info_hash"`
	Category string    `json:"category"`
	State    string    `json:"state"`
	Size     int64     `json:"size"`
	Progress float64   `json:"progress"`
	AddedOn  time.Time `json:"added_on"`
	Debrid   string    `json:"debrid"`
}

type TorrentListResponse struct {
	Torrents []*TorrentEntry `json:"torrents"`
	Total    int             `json:"total"`
}

func (c *DecypharrClient) ListTorrents(ctx context.Context, search, state string) ([]*TorrentEntry, error) {
	u, _ := url.Parse(c.base + "/api/torrents")
	q := u.Query()
	if search != "" {
		q.Set("search", search)
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("limit", "100")
	u.RawQuery = q.Encode()

	var resp TorrentListResponse
	if err := c.get(ctx, u.String(), &resp); err != nil {
		// A 404 here means decypharr has no matching torrents — that is a valid
		// "no results" answer, not a diagnostic failure. Return an empty list so
		// the agent keeps investigating instead of aborting on the error.
		if errors.Is(err, ErrNotFound) {
			return []*TorrentEntry{}, nil
		}
		return nil, err
	}
	return resp.Torrents, nil
}

type RepairRunResponse struct {
	RunID string `json:"run_id"`
}

// RefreshLinks triggers a repair sweep with unrestrict_link=true, which
// causes decypharr to re-unrestrict all download URLs for broken entries.
func (c *DecypharrClient) RefreshLinks(ctx context.Context) (string, error) {
	body := map[string]any{
		"unrestrict_link": true,
		"auto_repair":     true,
	}
	var resp RepairRunResponse
	if err := c.post(ctx, "/api/repair/run", body, &resp); err != nil {
		return "", err
	}
	return resp.RunID, nil
}

// RunRepairSweep triggers a general repair sweep.
func (c *DecypharrClient) RunRepairSweep(ctx context.Context) (string, error) {
	body := map[string]any{
		"auto_repair": true,
	}
	var resp RepairRunResponse
	if err := c.post(ctx, "/api/repair/run", body, &resp); err != nil {
		return "", err
	}
	return resp.RunID, nil
}

// RecheckMedia asks decypharr to recheck a specific arr media item and
// optionally apply fixes.
func (c *DecypharrClient) RecheckMedia(ctx context.Context, arrName, mediaID string, fix bool) error {
	body := map[string]any{
		"arr":      arrName,
		"media_id": mediaID,
		"fix":      fix,
	}
	return c.post(ctx, "/api/repair/recheck/media", body, nil)
}

// RecheckEntry rechecks a specific named entry.
func (c *DecypharrClient) RecheckEntry(ctx context.Context, name string, fix bool) error {
	u := fmt.Sprintf("/api/repair/health/%s/check", url.PathEscape(name))
	if fix {
		u += "?fix=true"
	}
	return c.post(ctx, u, nil, nil)
}

// DeleteTorrent removes a torrent, optionally deleting it from the debrid provider.
func (c *DecypharrClient) DeleteTorrent(ctx context.Context, category, hash string, removeFromDebrid bool) error {
	u := fmt.Sprintf("%s/api/torrents/%s/%s", c.base, url.PathEscape(category), url.PathEscape(hash))
	if removeFromDebrid {
		u += "?removeFromDebrid=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("decypharr DELETE %s: status %d", u, resp.StatusCode)
	}
	return nil
}

// Restart triggers a decypharr restart via the qBittorrent-compatible API.
func (c *DecypharrClient) Restart(ctx context.Context) error {
	return c.post(ctx, "/api/v2/app/restart", nil, nil)
}

func (c *DecypharrClient) get(ctx context.Context, rawURL string, out any) error {
	u := rawURL
	if len(rawURL) > 0 && rawURL[0] == '/' {
		u = c.base + rawURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("decypharr GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("decypharr GET %s: %w", u, ErrNotFound)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("decypharr GET %s: status %d", u, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *DecypharrClient) post(ctx context.Context, path string, body, out any) error {
	u := c.base + path
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("decypharr POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("decypharr POST %s: status %d", path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *DecypharrClient) setAuth(req *http.Request) {
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
}
