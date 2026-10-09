package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type SeerrClient struct {
	base   string
	apiKey string
	http   *http.Client
}

func NewSeerr(baseURL, apiKey string) *SeerrClient {
	return &SeerrClient{
		base:   strings.TrimRight(baseURL, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: defaultHTTPTimeout},
	}
}

func (c *SeerrClient) Comment(ctx context.Context, issueID, message string) error {
	body, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return err
	}
	return c.post(ctx, issueID, "comment", body)
}

func (c *SeerrClient) Resolve(ctx context.Context, issueID string) error {
	return c.post(ctx, issueID, "resolved", nil)
}

func (c *SeerrClient) post(ctx context.Context, issueID, action string, body []byte) error {
	if _, err := strconv.ParseUint(issueID, 10, 64); err != nil {
		return fmt.Errorf("seerr: invalid issue id %q", issueID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/v1/issue/"+issueID+"/"+action, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("seerr %s issue %s: status %d", action, issueID, resp.StatusCode)
	}
	return nil
}
