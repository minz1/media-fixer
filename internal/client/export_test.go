package client

import "net/http"

func (c *LokiClient) HTTPClientForTest() *http.Client { return c.http }
