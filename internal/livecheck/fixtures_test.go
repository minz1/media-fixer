package livecheck_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/livecheck"
)

// TestDecypharrRepairRunning_RealShape locks in the confirmed live shape of
// decypharr's GET /api/repair/status, confirmed twice over against real
// behavior: a first attempt at this check parsed a nonexistent top-level
// "running" boolean (never tripped, letting two sweeps race). A second
// attempt parsed last_run.status == "running" — but decypharr's own source
// (pkg/manager/repair.go) explicitly excludes the active run when building
// last_run, so that field can never legitimately be "running"; the real
// in-progress signal is active_run being present and non-null.
func TestDecypharrRepairRunning_RealShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		raw            string
		wantRunning    bool
		wantRecognized bool
	}{
		{
			name: "active_run present",
			raw: `{"enabled":true,"active_run":{"id":"29dfb92b","status":"running","stage":"probing"},` +
				`"last_run":{"status":"completed","stage":"done"}}`,
			wantRunning:    true,
			wantRecognized: true,
		},
		{
			name:           "active_run explicit null",
			raw:            `{"enabled":true,"active_run":null,"last_run":{"status":"completed","stage":"done"}}`,
			wantRunning:    false,
			wantRecognized: true,
		},
		{
			name: "idle, no active_run key at all",
			raw: `{"enabled":true,"next_run_at":"2026-08-26T02:00:00Z",` +
				`"last_run":{"status":"completed","stage":"done"},"health_counts":{"ok":10}}`,
			wantRunning:    false,
			wantRecognized: true,
		},
		{
			// last_run.status can never actually be "running" per decypharr's
			// source, but the guard must not be fooled if it somehow were —
			// only active_run should determine the result.
			name:           "last_run.status running is not itself the signal",
			raw:            `{"enabled":true,"active_run":null,"last_run":{"status":"running"}}`,
			wantRunning:    false,
			wantRecognized: true,
		},
		{
			name:           "unrecognized shape",
			raw:            `["not", "an", "object"]`,
			wantRunning:    false,
			wantRecognized: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			running, recognized := livecheck.DecypharrRepairRunningForTest(json.RawMessage(tc.raw))
			if running != tc.wantRunning || recognized != tc.wantRecognized {
				t.Errorf(
					"got (running=%v, recognized=%v), want (%v, %v)",
					running, recognized, tc.wantRunning, tc.wantRecognized,
				)
			}
		})
	}
}

// TestFirstRepairEntryName_RealShape locks in the confirmed live shape of
// decypharr's GET /api/repair/health: a bare array of objects keyed by
// "entry_name", not "name"/"entry"/"id".
func TestFirstRepairEntryName_RealShape(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[
		{"entry_name": " [HatSubs] One Piece 1162 ", "protocol": "torrent", "status": "unknown"},
		{"entry_name": "Other Entry", "protocol": "torrent", "status": "ok"}
	]`)
	name, ok := livecheck.FirstRepairEntryNameForTest(raw)
	if !ok {
		t.Fatal("expected a name to be found")
	}
	if name != " [HatSubs] One Piece 1162 " {
		t.Errorf("got %q", name)
	}
}

// TestDecypharrActiveRunStageSuffix_RealShape verifies the decypharr_repair_sweep
// skip message can tell a near-instant no-op sweep (0 candidates due, common
// since decypharr's recheck_interval is measured in days) apart from a real
// multi-minute one by surfacing active_run.stage — confirmed live to matter:
// nearly every sweep observed during heavy repair-endpoint testing was the
// fast due=0 path, which "a decypharr repair is already running" alone reads
// as more significant than it is.
func TestDecypharrActiveRunStageSuffix_RealShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "active run with stage",
			raw:  `{"active_run":{"id":"29dfb92b","status":"running","stage":"probing"}}`,
			want: " (stage: probing)",
		},
		{
			name: "no active run",
			raw:  `{"active_run":null,"last_run":{"status":"completed"}}`,
			want: "",
		},
		{
			name: "active run without a stage field",
			raw:  `{"active_run":{"id":"29dfb92b","status":"running"}}`,
			want: "",
		},
		{
			name: "unrecognized shape",
			raw:  `["not","an","object"]`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := livecheck.DecypharrActiveRunStageSuffixForTest(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// jellyfinFixtureServer serves a Sonarr-known series "The Boys" (id 7) whose
// Jellyfin item is a Series with two episodes, so playback-item resolution
// has something real to pick a first episode from.
func jellyfinFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/sonarr/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Series{{ID: 7, Title: "The Boys"}})
	})
	mux.HandleFunc("/radarr/api/v3/movie", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Movie{})
	})
	mux.HandleFunc("/decypharr/api/torrents", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.TorrentListResponse{})
	})
	mux.HandleFunc("/jellyfin/Items", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("searchTerm") != "The Boys" {
			t.Errorf("unexpected search term: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{
			Items: []client.JellyfinItem{{ID: "series-1", Name: "The Boys", Type: "Series"}},
		})
	})
	mux.HandleFunc("/jellyfin/Shows/series-1/Episodes", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{
			Items: []client.JellyfinItem{
				{ID: "ep-1", Name: "Episode 1", Type: "Episode"},
				{ID: "ep-2", Name: "Episode 2", Type: "Episode"},
			},
		})
	})

	return httptest.NewServer(mux)
}

// TestDiscoverFixtures_PlaybackItem_SeriesResolvesToEpisode is a regression
// test for a real 500: jellyfin_playback_info called directly on a Series ID
// throws (InvalidCastException: Series -> IHasMediaSources) on at least one
// live Jellyfin build, even when the series has episodes indexed. Fixture
// discovery must resolve a playback-safe (episode) item ID instead of
// reusing the series ID.
func TestDiscoverFixtures_PlaybackItem_SeriesResolvesToEpisode(t *testing.T) {
	t.Parallel()
	srv := jellyfinFixtureServer(t)
	defer srv.Close()

	loki, err := client.NewLoki(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	disp := &agent.Dispatcher{
		Sonarr:    client.NewArr(srv.URL+"/sonarr", "key"),
		Radarr:    client.NewArr(srv.URL+"/radarr", "key"),
		Decypharr: client.NewDecypharr(srv.URL+"/decypharr", "token"),
		Jellyfin:  client.NewJellyfin(srv.URL+"/jellyfin", "key"),
		Loki:      loki,
	}

	report := livecheck.New(disp, livecheck.Options{}).Run(context.Background())

	if report.Fixtures.JellyfinItemID != "series-1" {
		t.Errorf("JellyfinItemID = %q, want series-1", report.Fixtures.JellyfinItemID)
	}
	if report.Fixtures.JellyfinPlaybackItemID != "ep-1" {
		t.Errorf(
			"JellyfinPlaybackItemID = %q, want ep-1 (first episode, not the series)",
			report.Fixtures.JellyfinPlaybackItemID,
		)
	}
}

// TestDiscoverFixtures_PlaybackItem_NonSeriesPassesThrough verifies a
// non-Series item (which PlaybackInfo can be called on directly) is used
// as-is rather than triggering an unnecessary episode lookup.
func TestDiscoverFixtures_PlaybackItem_NonSeriesPassesThrough(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/sonarr/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Series{{ID: 7, Title: "The Boys"}})
	})
	mux.HandleFunc("/radarr/api/v3/movie", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Movie{})
	})
	mux.HandleFunc("/decypharr/api/torrents", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.TorrentListResponse{})
	})
	mux.HandleFunc("/jellyfin/Items", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{
			Items: []client.JellyfinItem{{ID: "movie-1", Name: "The Boys", Type: "Movie"}},
		})
	})
	// Not stubbing /jellyfin/Shows/movie-1/Episodes on purpose: the unrelated
	// jellyfin_list_episodes check calls it too (for any item ID, Series or
	// not), so asserting "never called" here would false-positive on that.
	// The real assertion below (JellyfinPlaybackItemID == movie-1, not
	// empty) already proves discovery didn't need it: if the non-Series
	// branch had incorrectly gone down the episode-lookup path, that lookup
	// would 404 and leave JellyfinPlaybackItemID unset via fx.missing.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	loki, err := client.NewLoki(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	disp := &agent.Dispatcher{
		Sonarr:    client.NewArr(srv.URL+"/sonarr", "key"),
		Radarr:    client.NewArr(srv.URL+"/radarr", "key"),
		Decypharr: client.NewDecypharr(srv.URL+"/decypharr", "token"),
		Jellyfin:  client.NewJellyfin(srv.URL+"/jellyfin", "key"),
		Loki:      loki,
	}

	report := livecheck.New(disp, livecheck.Options{}).Run(context.Background())

	if report.Fixtures.JellyfinPlaybackItemID != "movie-1" {
		t.Errorf("JellyfinPlaybackItemID = %q, want movie-1", report.Fixtures.JellyfinPlaybackItemID)
	}
}
