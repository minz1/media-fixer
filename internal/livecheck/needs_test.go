package livecheck_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/clientset"
	"github.com/minz1/mediafixer/internal/livecheck"
)

func runTool(t *testing.T, disp *agent.Dispatcher, tool string) livecheck.Result {
	t.Helper()
	opts := livecheck.Options{AllowWrite: true, AllowDisruptive: true, Only: []string{tool}}
	report := livecheck.New(disp, opts).Run(context.Background())
	if len(report.Results) != 1 {
		t.Fatalf("%s: %d results, want 1", tool, len(report.Results))
	}
	return report.Results[0]
}

func TestD11_3_UnconfiguredServicesShowUnconfigured(t *testing.T) {
	t.Parallel()
	disp := &agent.Dispatcher{
		Decypharr: client.NewDecypharr("", "k"),
		Jellyfin:  client.NewJellyfin("", "k"),
		Sonarr:    client.NewArr("", "k"),
		Radarr:    client.NewArr("", "k"),
	}
	want := map[string]string{
		"jellyfin_search":         "jellyfin not configured",
		"jellyfin_playback_info":  "jellyfin not configured",
		"jellyfin_list_episodes":  "jellyfin not configured",
		"stream_alignment":        "jellyfin not configured",
		"subtitle_extract_test":   "jellyfin not configured",
		"jellyfin_scan_status":    "jellyfin not configured",
		"jellyfin_library_scan":   "jellyfin not configured",
		"clear_jellyfin_cache":    "jellyfin not configured",
		"get_torrent_state":       "decypharr not configured",
		"get_repair_status":       "decypharr not configured",
		"get_repair_health":       "decypharr not configured",
		"refresh_decypharr_links": "decypharr not configured",
		"decypharr_repair_sweep":  "decypharr not configured",
		"decypharr_cache_cleanup": "decypharr not configured",
		"decypharr_recheck":       "decypharr not configured",
		"decypharr_delete_readd":  "decypharr not configured",
		"arr_media_status":        "sonarr and radarr not configured",
		"arr_grab_history":        "sonarr and radarr not configured",
		"arr_search_missing":      "sonarr and radarr not configured",
		"arr_remove_and_search":   "sonarr and radarr not configured",
		"sonarr_rescan":           "sonarr not configured",
		"radarr_rescan":           "radarr not configured",
		"dd_readability_test":     "media-agent not configured",
		"probe_streams":           "media-agent not configured",
		"list_directory":          "media-agent not configured",
		"get_disk_info":           "media-agent not configured",
		"restart_decypharr":       "media-agent not configured",
		"restart_jellyfin":        "media-agent not configured",
		"loki_query":              "loki not configured; log search unavailable",
	}
	for tool, detail := range want {
		got := runTool(t, disp, tool)
		if got.Status != livecheck.StatusUnconfigured || got.Detail != detail {
			t.Errorf("%s: status = %s, detail = %q, error = %q, want unconfigured %q",
				tool, got.Status, got.Detail, got.Error, detail)
		}
	}
}

func TestD11_3_LokiCertFailureStaysFail(t *testing.T) {
	t.Parallel()
	loadErr := errors.New("loki mTLS: load keypair: no such file")
	base := func() *agent.Dispatcher {
		return &agent.Dispatcher{
			Decypharr: client.NewDecypharr("", "k"),
			Jellyfin:  client.NewJellyfin("", "k"),
			Sonarr:    client.NewArr("", "k"),
			Radarr:    client.NewArr("", "k"),
		}
	}
	set := clientset.Set{
		Decypharr: client.NewDecypharr("", "k"), Jellyfin: client.NewJellyfin("", "k"),
		Sonarr: client.NewArr("", "k"), Radarr: client.NewArr("", "k"), LokiErr: loadErr,
	}
	got := runTool(t, set.Dispatcher(), "loki_query")
	if got.Status != livecheck.StatusFail || !strings.Contains(got.Error, loadErr.Error()) {
		t.Errorf("cert failure: status = %s, error = %q, want fail with %q", got.Status, got.Error, loadErr)
	}

	got = runTool(t, base(), "loki_query")
	if got.Status != livecheck.StatusUnconfigured {
		t.Errorf("empty URL: status = %s, detail = %q, error = %q, want unconfigured",
			got.Status, got.Detail, got.Error)
	}
}
