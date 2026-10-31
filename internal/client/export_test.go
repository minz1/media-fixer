package client

import (
	"net/http"
	"time"
)

func (c *LokiClient) HTTPClientForTest() *http.Client { return c.http }

func (c *MediaAgentClient) SetStuckReadsTimeoutForTest(d time.Duration) { c.stuckHTTP.Timeout = d }
