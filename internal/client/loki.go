package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type LokiClient struct {
	base string
	http *http.Client
}

func NewLoki(base, tlsCert, tlsKey string) (*LokiClient, error) {
	httpClient := &http.Client{Timeout: defaultHTTPTimeout}

	if tlsCert != "" && tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			return nil, fmt.Errorf("loki mTLS: load keypair: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("loki mTLS: system cert pool: %w", err)
		}
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
			},
		}
	}

	return &LokiClient{base: base, http: httpClient}, nil
}

type LokiQueryResult struct {
	Lines []LokiLine
}

type LokiLine struct {
	Timestamp time.Time
	Message   string
}

// QueryRange queries Loki for log lines matching logQL in the given time window.
func (c *LokiClient) QueryRange(
	ctx context.Context,
	logQL string,
	from, to time.Time,
	limit int,
) (*LokiQueryResult, error) {
	u, _ := url.Parse(c.base + "/loki/api/v1/query_range")
	q := u.Query()
	q.Set("query", logQL)
	q.Set("start", strconv.FormatInt(from.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(to.UnixNano(), 10))
	q.Set("limit", strconv.Itoa(limit))
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
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("loki query: status %d", resp.StatusCode)
	}

	var raw struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"` // [[ns_timestamp, line], ...]
			} `json:"result"`
		} `json:"data"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&raw); decodeErr != nil {
		return nil, fmt.Errorf("loki decode: %w", decodeErr)
	}

	result := &LokiQueryResult{}
	for _, stream := range raw.Data.Result {
		for _, v := range stream.Values {
			const lokiValueFields = 2
			if len(v) < lokiValueFields {
				continue
			}
			nsec, parseErr := strconv.ParseInt(v[0], 10, 64)
			if parseErr != nil {
				continue
			}
			result.Lines = append(result.Lines, LokiLine{
				Timestamp: time.Unix(0, nsec),
				Message:   v[1],
			})
		}
	}
	return result, nil
}
