package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type LokiClient struct {
	base string
	http *http.Client
}

func NewLoki(base string) *LokiClient {
	return &LokiClient{
		base: base,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

type LokiQueryResult struct {
	Lines []LokiLine
}

type LokiLine struct {
	Timestamp time.Time
	Message   string
}

// QueryRange queries Loki for log lines matching logQL in the given time window.
// units is a LogQL stream selector, e.g. `{unit=~"jellyfin|decypharr"}`.
func (c *LokiClient) QueryRange(ctx context.Context, logQL string, from, to time.Time, limit int) (*LokiQueryResult, error) {
	u, _ := url.Parse(c.base + "/loki/api/v1/query_range")
	q := u.Query()
	q.Set("query", logQL)
	q.Set("start", fmt.Sprintf("%d", from.UnixNano()))
	q.Set("end", fmt.Sprintf("%d", to.UnixNano()))
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("direction", "backward")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("loki query: status %d", resp.StatusCode)
	}

	var raw struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"` // [[ns_timestamp, line], ...]
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("loki decode: %w", err)
	}

	result := &LokiQueryResult{}
	for _, stream := range raw.Data.Result {
		for _, v := range stream.Values {
			if len(v) < 2 {
				continue
			}
			var nsec int64
			fmt.Sscanf(v[0], "%d", &nsec)
			result.Lines = append(result.Lines, LokiLine{
				Timestamp: time.Unix(0, nsec),
				Message:   v[1],
			})
		}
	}
	return result, nil
}
