package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

func TestG6_SubtitlesRefusesServiceWideActions(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	t.Cleanup(srv.Close)
	disp := &agent.Dispatcher{
		Jellyfin:   client.NewJellyfin(srv.URL, "key"),
		MediaAgent: client.NewMediaAgent(srv.URL, ""),
	}
	for _, tool := range []string{"restart_jellyfin", "jellyfin_library_scan"} {
		_, err := disp.CallScoped(context.Background(), agent.Scope{What: agent.WhatSubtitles}, tool, map[string]any{})
		if err == nil {
			t.Errorf("%s ran for what=subtitles", tool)
		}
		if hits.Load() != 0 {
			t.Fatalf("%s reached Jellyfin/media-agent for what=subtitles", tool)
		}
	}
	if _, err := disp.CallScoped(
		context.Background(), agent.Scope{What: "cant_play"}, "jellyfin_library_scan", map[string]any{},
	); err != nil {
		t.Errorf("cant_play library scan: %v", err)
	}
	if hits.Load() == 0 {
		t.Error("cant_play library scan never reached Jellyfin")
	}
}
