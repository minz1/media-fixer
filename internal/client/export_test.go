package client

import (
	"net/http"
	"time"
)

func (c *LokiClient) HTTPClientForTest() *http.Client { return c.http }

func (c *MediaAgentClient) SetStuckReadsTimeoutForTest(d time.Duration) { c.stuckTimeout = d }

func (c *JellyfinClient) SetSubtitleTimeoutForTest(d time.Duration) { c.subtitleTimeout = d }

func (c *JellyfinClient) SubtitleHTTPClientForTest() *http.Client { return c.subtitleHTTP }
