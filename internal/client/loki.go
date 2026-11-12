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
	base    string
	http    *http.Client
	loadErr error
}

func NewLoki(base, tlsCert, tlsKey string) (*LokiClient, error) {
	httpClient := newHTTPClient(defaultHTTPTimeout)

	if tlsCert != "" && tlsKey != "" {
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			return nil, fmt.Errorf("loki mTLS: load keypair: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("loki mTLS: system cert pool: %w", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
		}
		if tr, ok := httpClient.Transport.(*http.Transport); ok {
			tr.TLSClientConfig = tlsConfig
		} else {
			httpClient.Transport = &http.Transport{TLSClientConfig: tlsConfig}
		}
	}

	return &LokiClient{base: base, http: httpClient}, nil
}

func NewLokiUnavailable(err error) *LokiClient { return &LokiClient{loadErr: err} }

func (c *LokiClient) Configured() bool { return c != nil && c.base != "" }

func (c *LokiClient) Err() error {
	if c == nil {
		return nil
	}
	return c.loadErr
}

type LokiQueryResult struct {
	Lines []LokiLine
}

type LokiLine struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Context   bool      `json:"context,omitempty"`
	Unit      string    `json:"-"`
}

// QueryRange queries Loki for log lines matching logQL in the given time window, newest first.
func (c *LokiClient) QueryRange(
	ctx context.Context,
	logQL string,
	from, to time.Time,
	limit int,
) (*LokiQueryResult, error) {
	return c.query(ctx, logQL, from, to, limit, "backward")
}

// QueryForward is QueryRange with the oldest lines first.
func (c *LokiClient) QueryForward(
	ctx context.Context,
	logQL string,
	from, to time.Time,
	limit int,
) (*LokiQueryResult, error) {
	return c.query(ctx, logQL, from, to, limit, "forward")
}

func (c *LokiClient) query(
	ctx context.Context,
	logQL string,
	from, to time.Time,
	limit int,
	direction string,
) (*LokiQueryResult, error) {
	u, _ := url.Parse(c.base + "/loki/api/v1/query_range")
	q := u.Query()
	q.Set("query", logQL)
	q.Set("start", strconv.FormatInt(from.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(to.UnixNano(), 10))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("direction", direction)
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
				Stream map[string]string `json:"stream"`
				Values [][]string        `json:"values"` // [[ns_timestamp, line], ...]
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
				Unit:      stream.Stream["unit"],
			})
		}
	}
	return result, nil
}
