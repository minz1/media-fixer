package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func promptBetween(t *testing.T, prompt, start, end string) string {
	t.Helper()
	_, after, found := strings.Cut(prompt, start)
	if !found {
		t.Fatalf("system prompt has no %q", start)
	}
	section, _, _ := strings.Cut(after, end)
	return section
}

func TestG1_PromptHasSubtitleProtocol(t *testing.T) {
	t.Parallel()
	prompt := agent.SystemPromptForTest()
	step1 := promptBetween(t, prompt, "Step 1 — Jellyfin lookup", "Step 2 —")
	for _, want := range []string{"jellyfin_list_episodes", "ParentIndexNumber", "item_ids"} {
		if !strings.Contains(step1, want) {
			t.Errorf("step 1 missing %q", want)
		}
	}
	step5 := promptBetween(t, prompt, "Step 5 — Log review", "After all five steps")
	for _, want := range []string{"filters", "context_after: 20"} {
		if !strings.Contains(step5, want) {
			t.Errorf("step 5 missing %q", want)
		}
	}
	sub := promptBetween(t, prompt, "--- Subtitles (what=subtitles) ---", "\n---")
	for _, want := range []string{"context_after", "dd_readability_test", "manual_investigation", "clear_jellyfin_cache"} {
		if !strings.Contains(sub, want) {
			t.Errorf("subtitles section missing %q", want)
		}
	}
	rule := promptBetween(t, prompt, "A deterministic per-file error", "\n\n")
	for _, want := range []string{"restart_jellyfin", "restart_decypharr", "jellyfin_library_scan", "decypharr_cache_cleanup"} {
		if !strings.Contains(rule, want) {
			t.Errorf("per-file rule missing %q", want)
		}
	}
}

func TestB2_ProbeStreamsRefusedOnStuckRead(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"streams": []any{}})
	}))
	t.Cleanup(srv.Close)
	disp := &agent.Dispatcher{MediaAgent: client.NewMediaAgent(srv.URL, "")}
	args := map[string]any{"file_path": "/mnt/x.mkv"}

	_, err := disp.CallScoped(context.Background(), agent.Scope{What: agent.WhatStuckRead}, "probe_streams", args)
	if err == nil || !strings.Contains(err.Error(), "probe_streams is disabled for stuck_read incidents") {
		t.Fatalf("stuck_read err = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("stuck_read reached media-agent: %d requests", hits.Load())
	}
	if _, err = disp.CallScoped(
		context.Background(),
		agent.Scope{What: agent.WhatSubtitles},
		"probe_streams",
		args,
	); err != nil {
		t.Fatalf("subtitles: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("subtitles requests = %d, want 1", hits.Load())
	}
}
