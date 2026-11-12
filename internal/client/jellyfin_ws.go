package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	jellyfinScanKey         = "RefreshLibrary"
	jellyfinWSMinBackoff    = time.Second
	jellyfinWSMaxBackoff    = 5 * time.Minute
	jellyfinWSBackoffFactor = 2
	jellyfinWSKeepAlive     = 30 * time.Second
	jellyfinWSWriteWait     = 10 * time.Second
	jellyfinWSDialTimeout   = 15 * time.Second
	jellyfinWSReadWait      = time.Minute
	jellyfinTasksInterval   = "0,10000"
)

type JellyfinWSState struct {
	Connected bool
	Err       string
	RetryAt   time.Time
}

type JellyfinWS struct {
	base   string
	apiKey string

	OnScanFinished func(ctx context.Context, ended time.Time)
	Sleep          func(ctx context.Context, d time.Duration) bool
	ReadWait       time.Duration

	mu    sync.Mutex
	state JellyfinWSState
	wg    sync.WaitGroup
}

func NewJellyfinWS(base, apiKey string) *JellyfinWS {
	return &JellyfinWS{base: base, apiKey: apiKey, Sleep: sleepCtx, ReadWait: jellyfinWSReadWait}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *JellyfinWS) Configured() bool { return c != nil && c.base != "" }

func (c *JellyfinWS) State() JellyfinWSState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *JellyfinWS) setState(s JellyfinWSState) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

func (c *JellyfinWS) Start(ctx context.Context) {
	if !c.Configured() {
		return
	}
	c.wg.Go(func() { c.loop(ctx) })
}

func (c *JellyfinWS) Wait() { c.wg.Wait() }

func (c *JellyfinWS) loop(ctx context.Context) {
	backoff := jellyfinWSMinBackoff
	for ctx.Err() == nil {
		started := time.Now()
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			break
		}
		if connected && time.Since(started) >= jellyfinWSKeepAlive {
			backoff = jellyfinWSMinBackoff
		}
		msg := "connection closed"
		if err != nil {
			msg = err.Error()
		}
		c.setState(JellyfinWSState{Err: msg, RetryAt: time.Now().Add(backoff)})
		if !c.Sleep(ctx, backoff) {
			break
		}
		backoff = min(backoff*jellyfinWSBackoffFactor, jellyfinWSMaxBackoff)
	}
	c.mu.Lock()
	c.state.Connected, c.state.RetryAt = false, time.Time{}
	c.mu.Unlock()
}

func (c *JellyfinWS) socketURL() (string, error) {
	u, err := url.Parse(strings.TrimRight(c.base, "/") + "/socket")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.RawQuery = url.Values{"deviceId": {"media-fixer"}}.Encode()
	return u.String(), nil
}

func (c *JellyfinWS) session(ctx context.Context) (bool, error) {
	target, err := c.socketURL()
	if err != nil {
		return false, err
	}
	hdr := http.Header{}
	hdr.Set("Authorization", `MediaBrowser Token="`+c.apiKey+`"`)
	dialer := websocket.Dialer{HandshakeTimeout: jellyfinWSDialTimeout, Proxy: http.ProxyFromEnvironment}
	conn, resp, err := dialer.DialContext(ctx, target, hdr)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return false, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer conn.Close()

	var writeMu sync.Mutex
	send := func(msgType string, data any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(jellyfinWSWriteWait))
		return conn.WriteJSON(jellyfinWSMessage{MessageType: msgType, Data: data})
	}
	if err = send("ScheduledTasksInfoStart", jellyfinTasksInterval); err != nil {
		return false, err
	}
	c.setState(JellyfinWSState{Connected: true})

	done := make(chan struct{})
	var keep sync.WaitGroup
	keep.Go(func() {
		t := time.NewTicker(jellyfinWSKeepAlive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if send("KeepAlive", nil) != nil {
					_ = conn.Close()
					return
				}
			}
		}
	})
	defer keep.Wait()
	defer close(done)

	var p jellyfinScanParser
	for {
		_ = conn.SetReadDeadline(time.Now().Add(c.ReadWait))
		_, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			return true, readErr
		}
		if ended, ok := p.parse(raw); ok && c.OnScanFinished != nil {
			c.OnScanFinished(ctx, ended)
		}
	}
}

type jellyfinWSMessage struct {
	MessageType string `json:"MessageType"`
	Data        any    `json:"Data,omitempty"`
}

type jellyfinTaskResult struct {
	EndTimeUtc string `json:"EndTimeUtc"`
	Key        string `json:"Key"`
}

func jellyfinUTC(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02T15:04:05.9999999", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

type jellyfinTaskInfo struct {
	State               string              `json:"State"`
	Key                 string              `json:"Key"`
	LastExecutionResult *jellyfinTaskResult `json:"LastExecutionResult"`
}

type jellyfinScanParser struct {
	running   bool
	lastEnded time.Time
}

func (p *jellyfinScanParser) parse(raw []byte) (time.Time, bool) {
	var env struct {
		MessageType string          `json:"MessageType"`
		Data        json.RawMessage `json:"Data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return time.Time{}, false
	}
	switch env.MessageType {
	case "ScheduledTaskEnded":
		var r jellyfinTaskResult
		if json.Unmarshal(env.Data, &r) != nil || r.Key != jellyfinScanKey {
			return time.Time{}, false
		}
		p.running = false
		return p.report(jellyfinUTC(r.EndTimeUtc))
	case "ScheduledTasksInfo":
		var tasks []jellyfinTaskInfo
		if json.Unmarshal(env.Data, &tasks) != nil {
			return time.Time{}, false
		}
		for _, t := range tasks {
			if t.Key != jellyfinScanKey {
				continue
			}
			if t.State == "Running" {
				p.running = true
				return time.Time{}, false
			}
			if !p.running || t.LastExecutionResult == nil {
				return time.Time{}, false
			}
			p.running = false
			return p.report(jellyfinUTC(t.LastExecutionResult.EndTimeUtc))
		}
	}
	return time.Time{}, false
}

func (p *jellyfinScanParser) report(ended time.Time) (time.Time, bool) {
	if ended.IsZero() {
		ended = time.Now()
	}
	if ended.Equal(p.lastEnded) {
		return time.Time{}, false
	}
	p.lastEnded = ended
	return ended, true
}
