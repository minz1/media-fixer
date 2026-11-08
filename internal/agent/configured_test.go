package agent_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

func TestD11_3_ToolsRefuseUnconfiguredDecypharr(t *testing.T) {
	t.Parallel()
	disp := &agent.Dispatcher{Decypharr: client.NewDecypharr("", "k")}
	const want = "decypharr not configured"
	for _, tool := range []string{
		"get_torrent_state", "get_repair_status", "get_repair_health", "decypharr_delete_readd",
	} {
		_, err := disp.Call(context.Background(), tool, map[string]any{"name": "x"})
		if err == nil || err.Error() != want {
			t.Errorf("%s: error = %v, want %q", tool, err, want)
		}
	}
}

func TestD11_3_FetchQueueUnconfiguredArrStillErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		disp *agent.Dispatcher
	}{
		{"nil clients", &agent.Dispatcher{}},
		{"empty urls", &agent.Dispatcher{Sonarr: client.NewArr("", "k"), Radarr: client.NewArr("", "k")}},
	}
	for _, tc := range cases {
		ag := agent.New(nil, "stub", tc.disp, nil, nil, slog.New(slog.DiscardHandler))
		for mediaType, want := range map[string]string{
			client.ReplaceMediaTV:    "sonarr not configured",
			client.ReplaceMediaMovie: "radarr not configured",
		} {
			queue, err := ag.FetchQueueForTest(context.Background(), mediaType)
			if err == nil || err.Error() != want || queue != nil {
				t.Errorf("%s/%s: queue = %v, err = %v, want error %q", tc.name, mediaType, queue, err, want)
			}
		}
	}
}
