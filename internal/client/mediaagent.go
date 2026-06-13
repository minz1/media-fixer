package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

// MediaAgentClient calls the media-agent sidecar running on minz-media-0.
type MediaAgentClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewMediaAgent(baseURL, apiKey string) *MediaAgentClient {
	return &MediaAgentClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{},
	}
}

func (c *MediaAgentClient) DDReadabilityTest(ctx context.Context, path string) (*mediaagentapi.DDTestResult, error) {
	body, _ := json.Marshal(mediaagentapi.DDTestRequest{Path: path})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/dd-test", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.auth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media-agent dd-test: status %d", resp.StatusCode)
	}
	var result mediaagentapi.DDTestResult
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return nil, fmt.Errorf("media-agent dd-test decode: %w", decodeErr)
	}
	return &result, nil
}

func (c *MediaAgentClient) RestartService(ctx context.Context, service string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/restart/"+service, nil)
	if err != nil {
		return err
	}
	c.auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e mediaagentapi.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return fmt.Errorf("media-agent restart %s: %s", service, e.Error)
		}
		return fmt.Errorf("media-agent restart %s: status %d", service, resp.StatusCode)
	}
	return nil
}

func (c *MediaAgentClient) DiskUsage(ctx context.Context) (*mediaagentapi.DiskResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/disk", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media-agent disk: status %d", resp.StatusCode)
	}
	var result mediaagentapi.DiskResult
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return nil, fmt.Errorf("media-agent disk decode: %w", decodeErr)
	}
	return &result, nil
}

func (c *MediaAgentClient) ListDirectory(ctx context.Context, path string) (*mediaagentapi.ListDirResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/ls?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e mediaagentapi.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return nil, fmt.Errorf("media-agent ls: %s", e.Error)
		}
		return nil, fmt.Errorf("media-agent ls: status %d", resp.StatusCode)
	}
	var result mediaagentapi.ListDirResult
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return nil, fmt.Errorf("media-agent ls decode: %w", decodeErr)
	}
	return &result, nil
}

func (c *MediaAgentClient) auth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}
