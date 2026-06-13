package client

import (
	"errors"
	"time"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("not found")

// defaultHTTPTimeout is applied to all client HTTP connections.
const defaultHTTPTimeout = 30 * time.Second
