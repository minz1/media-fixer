package client

import (
	"errors"
	"net/http"
	"time"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("not found")

// defaultHTTPTimeout is applied to all client HTTP connections.
const defaultHTTPTimeout = 30 * time.Second

func newHTTPClient(timeout time.Duration) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Timeout: timeout, Transport: base.Clone()}
}
