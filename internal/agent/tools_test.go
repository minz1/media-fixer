package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

func TestFixLokiUnitSelector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "regex bare names get service suffix",
			input: `{unit=~"jellyfin|decypharr"}`,
			want:  `{unit=~"jellyfin\.service|decypharr\.service"}`,
		},
		{
			name:  "exact bare name gets service suffix",
			input: `{unit="jellyfin"}`,
			want:  `{unit="jellyfin.service"}`,
		},
		{
			name:  "regex already correct dot-escaped unchanged",
			input: `{unit=~"jellyfin\.service|decypharr\.service"}`,
			want:  `{unit=~"jellyfin\.service|decypharr\.service"}`,
		},
		{
			name:  "regex already correct unescaped dot unchanged",
			input: `{unit=~"jellyfin.service|decypharr.service"}`,
			want:  `{unit=~"jellyfin.service|decypharr.service"}`,
		},
		{
			name:  "exact already correct unchanged",
			input: `{unit="jellyfin.service"}`,
			want:  `{unit="jellyfin.service"}`,
		},
		{
			name:  "mixed: only bare name gets fixed",
			input: `{unit=~"jellyfin|decypharr.service"}`,
			want:  `{unit=~"jellyfin\.service|decypharr.service"}`,
		},
		{
			name:  "non-unit selector unchanged",
			input: `{job="systemd-journal"}`,
			want:  `{job="systemd-journal"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := agent.FixLokiUnitSelector(tc.input)
			if got != tc.want {
				t.Errorf("fixLokiUnitSelector(%q)\n got  %q\n want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestWaitUntilReady is a regression test for restart_jellyfin/restart_decypharr
// reporting "restarted" as soon as systemd's restart command returns, even
// though the service inside hadn't finished starting yet (confirmed live:
// restart_jellyfin reported ok in 3.1s, but Jellyfin didn't accept
// connections until ~6s in, and the very next tool call in the same run hit
// the gap and got connection-refused).
func TestWaitUntilReady(t *testing.T) {
	t.Parallel()

	t.Run("succeeds immediately", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		probe := func(context.Context) error {
			calls.Add(1)
			return nil
		}
		err := agent.WaitUntilReadyForTest(context.Background(), time.Second, time.Millisecond, probe)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls.Load() != 1 {
			t.Errorf("probe called %d times, want 1", calls.Load())
		}
	})

	t.Run("succeeds after a few attempts within the timeout", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		probe := func(context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("not ready yet")
			}
			return nil
		}
		err := agent.WaitUntilReadyForTest(context.Background(), time.Second, time.Millisecond, probe)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if calls.Load() != 3 {
			t.Errorf("probe called %d times, want 3", calls.Load())
		}
	})

	t.Run("returns the last error if it never succeeds", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("still down")
		probe := func(context.Context) error { return wantErr }
		err := agent.WaitUntilReadyForTest(context.Background(), 30*time.Millisecond, 10*time.Millisecond, probe)
		if !errors.Is(err, wantErr) {
			t.Errorf("got %v, want %v", err, wantErr)
		}
	})

	t.Run("returns ctx error if canceled while waiting", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		probe := func(context.Context) error { return errors.New("still down") }
		err := agent.WaitUntilReadyForTest(ctx, time.Second, time.Millisecond, probe)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	})
}

// TestDispatchRestartJellyfin_WaitsForReady verifies restart_jellyfin only
// reports success once Jellyfin is actually responding, not as soon as
// systemctl's restart call returns.
func TestDispatchRestartJellyfin_WaitsForReady(t *testing.T) {
	t.Parallel()
	var pingCalls atomic.Int32
	jellyfin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Ping" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if pingCalls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer jellyfin.Close()

	mediaAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/restart/jellyfin" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mediaAgent.Close()

	disp := &agent.Dispatcher{
		Jellyfin:   client.NewJellyfin(jellyfin.URL, "key"),
		MediaAgent: client.NewMediaAgent(mediaAgent.URL, "key"),
	}
	result, err := disp.Call(context.Background(), "restart_jellyfin", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pingCalls.Load() != 3 {
		t.Errorf("Ping called %d times, want 3 (waited for readiness)", pingCalls.Load())
	}
	if m, ok := result.(map[string]string); !ok || m["status"] != "restarted" {
		t.Errorf("result = %+v, want status=restarted", result)
	}
}

// arrCommandRecorder captures the body of the most recent POST
// /api/v3/command, so a test can assert exactly which search command (and
// with what target ID) arr_search_missing actually triggered. It also lets a
// test control what GET /api/v3/queue returns, since CheckPendingOutcome
// tests need to simulate a download appearing in the queue — or the queue
// endpoint itself failing.
type arrCommandRecorder struct {
	mu        sync.Mutex
	last      map[string]any
	queue     []client.QueueRecord
	queueFail bool
}

func (r *arrCommandRecorder) record(body map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = body
}

func (r *arrCommandRecorder) Last() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// SetQueue controls what GET /api/v3/queue returns for the rest of the test.
func (r *arrCommandRecorder) SetQueue(records []client.QueueRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = records
}

// SetQueueFail makes GET /api/v3/queue return 500 for the rest of the test,
// simulating a transient Sonarr/Radarr outage.
func (r *arrCommandRecorder) SetQueueFail(fail bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queueFail = fail
}

func (r *arrCommandRecorder) QueueFail() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queueFail
}

func (r *arrCommandRecorder) Queue() []client.QueueRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queue
}

// newArrStatusFixtureServer serves a series ("Rick and Morty", id 42) with
// three season-9 episodes, where episode 9 has no file — the exact shape of
// the Rick and Morty S09E09 misdiagnosis (dd_readability_test and
// list_directory both returned ENOENT, and the agent had no way to learn the
// episode was simply never downloaded) — plus a movie ("Arrival", id 5) with
// a file, all served from one mux since Sonarr/Radarr's v3 paths don't
// collide. The returned recorder captures any search command a test triggers.
func newArrStatusFixtureServer(t *testing.T) (*httptest.Server, *arrCommandRecorder) {
	t.Helper()
	mux := http.NewServeMux()
	rec := &arrCommandRecorder{}
	mux.HandleFunc("/api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.record(body)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v3/queue", func(w http.ResponseWriter, _ *http.Request) {
		if rec.QueueFail() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": rec.Queue()})
	})

	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Series{{ID: 42, Title: "Rick and Morty"}})
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("seriesId"); got != "42" {
			t.Errorf("unexpected episode query seriesId: %s", got)
		}
		_ = json.NewEncoder(w).Encode([]client.Episode{
			{ID: 901, SeriesID: 42, SeasonNumber: 9, EpisodeNumber: 7, EpisodeFileID: 7001, HasFile: true},
			{ID: 902, SeriesID: 42, SeasonNumber: 9, EpisodeNumber: 8, EpisodeFileID: 7002, HasFile: true},
			{ID: 903, SeriesID: 42, SeasonNumber: 9, EpisodeNumber: 9, HasFile: false},
		})
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.EpisodeFile{
			{ID: 7001, SeriesID: 42, Path: "/mnt/decypharr/__all__/RaM.S09/e07.mkv", Size: 100},
			{ID: 7002, SeriesID: 42, Path: "/mnt/decypharr/__all__/RaM.S09/e08.mkv", Size: 200},
		})
	})
	mux.HandleFunc("/api/v3/history/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{
			{ID: 2001, EpisodeID: 901, SeriesID: 42, EventType: "grabbed"},
			{ID: 2002, EpisodeID: 902, SeriesID: 42, EventType: "grabbed"},
		})
	})
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Movie{{ID: 5, Title: "Arrival", HasFile: true, MovieFileID: 500}})
	})
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).
			Encode([]client.MovieFile{{ID: 500, MovieID: 5, Path: "/data/library/movies/Arrival.mkv", Size: 999}})
	})
	mux.HandleFunc("/api/v3/history/movie", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{{ID: 3001, MovieID: 5, EventType: "grabbed"}})
	})

	return httptest.NewServer(mux), rec
}

// TestArrMediaStatus_MissingEpisode is the direct regression test for the
// Rick and Morty misdiagnosis: asking arr_media_status about the one missing
// episode of a season where every other episode has a file must report
// has_file=false for that episode specifically, not "series is fine".
func TestArrMediaStatus_MissingEpisode(t *testing.T) {
	t.Parallel()
	srv, _ := newArrStatusFixtureServer(t)
	defer srv.Close()

	disp := &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
	result, err := disp.Call(context.Background(), "arr_media_status", map[string]any{
		"media_type": "tv", "title": "Rick and Morty", "season": float64(9), "episode": float64(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, ok := result.(*agent.MediaStatusResult)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	if status.HasFile {
		t.Error("expected has_file=false for the missing episode")
	}
	if len(status.Episodes) != 3 {
		t.Fatalf("expected all 3 season-9 episodes in the response, got %d", len(status.Episodes))
	}
	var sawMissing bool
	for _, ep := range status.Episodes {
		if ep.Episode == 9 {
			sawMissing = true
			if ep.HasFile {
				t.Error("episode 9 reported has_file=true")
			}
		}
	}
	if !sawMissing {
		t.Error("episode 9 missing from the per-episode breakdown")
	}
}

// TestArrMediaStatus_SeasonQuery_FalseIfAnyEpisodeMissing covers the
// season-only (no episode) query used when the agent doesn't yet know which
// specific episode is bad: has_file must be false if ANY episode in the
// season lacks a file, not just true-if-any-has-one.
func TestArrMediaStatus_SeasonQuery_FalseIfAnyEpisodeMissing(t *testing.T) {
	t.Parallel()
	srv, _ := newArrStatusFixtureServer(t)
	defer srv.Close()

	disp := &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
	result, err := disp.Call(context.Background(), "arr_media_status", map[string]any{
		"media_type": "tv", "title": "Rick and Morty", "season": float64(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	status := result.(*agent.MediaStatusResult)
	if status.HasFile {
		t.Error("expected has_file=false for a season with a missing episode")
	}
}

// TestArrMediaStatus_Movie_HasFile covers the movie path end to end.
func TestArrMediaStatus_Movie_HasFile(t *testing.T) {
	t.Parallel()
	srv, _ := newArrStatusFixtureServer(t)
	defer srv.Close()

	disp := &agent.Dispatcher{Radarr: client.NewArr(srv.URL, "key")}
	result, err := disp.Call(context.Background(), "arr_media_status", map[string]any{
		"media_type": "movie", "title": "Arrival",
	})
	if err != nil {
		t.Fatal(err)
	}
	status := result.(*agent.MediaStatusResult)
	if !status.HasFile || status.Path != "/data/library/movies/Arrival.mkv" {
		t.Errorf("got %+v", status)
	}
}

// TestArrGrabHistory_TV covers the grab-history tool's TV path resolving
// title to series ID before querying history.
func TestArrGrabHistory_TV(t *testing.T) {
	t.Parallel()
	srv, _ := newArrStatusFixtureServer(t)
	defer srv.Close()

	disp := &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
	result, err := disp.Call(context.Background(), "arr_grab_history", map[string]any{
		"media_type": "tv", "title": "Rick and Morty", "season": float64(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	records, ok := result.([]client.HistoryRecord)
	if !ok || len(records) != 2 {
		t.Errorf("got %+v", result)
	}
}

// TestArrSearchMissing_RefusesWhenFileExists is the direct test of the
// tool's safety precondition: asking it to search for an episode that
// arr_media_status would report has_file=true must be refused with
// agent.ErrArrTargetHasFile, and must never reach the point of posting a
// search command.
func TestArrSearchMissing_RefusesWhenFileExists(t *testing.T) {
	t.Parallel()
	srv, rec := newArrStatusFixtureServer(t)
	defer srv.Close()

	disp := &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
	_, err := disp.Call(context.Background(), "arr_search_missing", map[string]any{
		"media_type": "tv", "title": "Rick and Morty", "season": float64(9), "episode": float64(7),
	})
	if !errors.Is(err, agent.ErrArrTargetHasFile) {
		t.Fatalf("got %v, want ErrArrTargetHasFile", err)
	}
	if rec.Last() != nil {
		t.Errorf("a search command was posted despite the refusal: %+v", rec.Last())
	}
}

// TestArrSearchMissing_TriggersEpisodeSearch is the direct regression test
// for the Rick and Morty fix: searching for the one missing episode must
// resolve its Sonarr episode ID (903, not the human-facing episode number 9)
// and post an EpisodeSearch command for exactly that ID.
func TestArrSearchMissing_TriggersEpisodeSearch(t *testing.T) {
	t.Parallel()
	srv, rec := newArrStatusFixtureServer(t)
	defer srv.Close()

	disp := &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
	result, err := disp.Call(context.Background(), "arr_search_missing", map[string]any{
		"media_type": "tv", "title": "Rick and Morty", "season": float64(9), "episode": float64(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := result.(map[string]any); !ok || m["scope"] != "episode" {
		t.Errorf("result = %+v, want scope=episode", result)
	}
	cmd := rec.Last()
	if cmd == nil || cmd["name"] != "EpisodeSearch" {
		t.Fatalf("command = %+v, want name=EpisodeSearch", cmd)
	}
	ids, ok := cmd["episodeIds"].([]any)
	if !ok || len(ids) != 1 || ids[0] != float64(903) {
		t.Errorf("episodeIds = %+v, want [903] (episode 9's Sonarr ID)", cmd["episodeIds"])
	}
}

// TestArrSearchMissing_SeasonScope covers the season-only (no episode) path.
func TestArrSearchMissing_SeasonScope(t *testing.T) {
	t.Parallel()
	srv, rec := newArrStatusFixtureServer(t)
	defer srv.Close()

	// Season 9 as a whole has a missing episode (9), so the season-level
	// aggregate has_file is false and the search should proceed.
	disp := &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
	result, err := disp.Call(context.Background(), "arr_search_missing", map[string]any{
		"media_type": "tv", "title": "Rick and Morty", "season": float64(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := result.(map[string]any); !ok || m["scope"] != "season" {
		t.Errorf("result = %+v, want scope=season", result)
	}
	cmd := rec.Last()
	if cmd == nil || cmd["name"] != "SeasonSearch" || cmd["seasonNumber"] != float64(9) {
		t.Errorf("command = %+v, want name=SeasonSearch seasonNumber=9", cmd)
	}
}

// TestArrSearchMissing_MovieScope covers the movie path, refused when a
// file exists.
func TestArrSearchMissing_MovieScope(t *testing.T) {
	t.Parallel()
	srv, _ := newArrStatusFixtureServer(t)
	defer srv.Close()

	disp := &agent.Dispatcher{Radarr: client.NewArr(srv.URL, "key")}
	_, err := disp.Call(context.Background(), "arr_search_missing", map[string]any{
		"media_type": "movie", "title": "Arrival",
	})
	if !errors.Is(err, agent.ErrArrTargetHasFile) {
		t.Fatalf("got %v, want ErrArrTargetHasFile (Arrival fixture already has a file)", err)
	}
}

// TestCheckPendingOutcome_HasFile_SkipsQueue confirms a target that already
// has a file reports HasFile without a QueueStage — CheckPendingOutcome must
// short-circuit before ever consulting the queue once the terminal signal is
// already available.
func TestCheckPendingOutcome_HasFile_SkipsQueue(t *testing.T) {
	t.Parallel()
	srv, rec := newArrStatusFixtureServer(t)
	defer srv.Close()
	// If CheckPendingOutcome queried the queue anyway despite HasFile being
	// true, this would still return no match (episode 7 isn't in it) — the
	// real assertion is QueueStage == "", proving the short-circuit.
	rec.SetQueue([]client.QueueRecord{{SeriesID: 999, Status: "downloading"}})

	a := agent.New(
		nil,
		"",
		&agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")},
		nil,
		nil,
		slog.New(slog.DiscardHandler),
	)
	obs, err := a.CheckPendingOutcome(context.Background(), &db.PendingOutcome{
		MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !obs.HasFile || obs.QueueStage != "" {
		t.Errorf("got %+v", obs)
	}
}

// TestCheckPendingOutcome_QueueMatch_ReportsStageAndProgress is the direct
// regression test for the "babysitting" mechanism: once a download is
// grabbed, the matching queue record's status and computed progress
// percentage must be surfaced, not just "still nothing" — this is what lets
// the sweeper send the "found it, downloading" milestone DM and detect a
// stall.
func TestCheckPendingOutcome_QueueMatch_ReportsStageAndProgress(t *testing.T) {
	t.Parallel()
	srv, rec := newArrStatusFixtureServer(t)
	defer srv.Close()
	rec.SetQueue([]client.QueueRecord{
		{SeriesID: 999, Status: "queued"},                                // different series: must not match
		{SeriesID: 42, Status: "downloading", Size: 1000, SizeLeft: 250}, // Rick and Morty: 75% done
	})

	a := agent.New(
		nil,
		"",
		&agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")},
		nil,
		nil,
		slog.New(slog.DiscardHandler),
	)
	obs, err := a.CheckPendingOutcome(context.Background(), &db.PendingOutcome{
		MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if obs.HasFile {
		t.Error("episode 9 has no file in the fixture; HasFile should be false")
	}
	if obs.QueueStage != "downloading" {
		t.Errorf("stage = %q, want downloading", obs.QueueStage)
	}
	const wantPct = 75.0
	if obs.ProgressPct != wantPct {
		t.Errorf("progress = %v, want %v", obs.ProgressPct, wantPct)
	}
}

// TestCheckPendingOutcome_NoMatchingQueueItem confirms an empty (or
// non-matching) queue reports neither HasFile nor a QueueStage — the "still
// waiting, nothing to report yet" case the sweeper's grace-period logic
// depends on.
func TestCheckPendingOutcome_NoMatchingQueueItem(t *testing.T) {
	t.Parallel()
	srv, _ := newArrStatusFixtureServer(t)
	defer srv.Close()

	a := agent.New(
		nil,
		"",
		&agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")},
		nil,
		nil,
		slog.New(slog.DiscardHandler),
	)
	obs, err := a.CheckPendingOutcome(context.Background(), &db.PendingOutcome{
		MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	if obs.HasFile || obs.QueueStage != "" {
		t.Errorf("got %+v", obs)
	}
}

// TestCheckPendingOutcome_QueueFetchFails_PropagatesError is the regression
// test for a latent false-escalation bug: CheckPendingOutcome used to
// swallow a queue-fetch failure and report HasFile:false, QueueStage:"" —
// indistinguishable from "genuinely nothing happening." If that hiccup landed
// on the exact poll where an incident's grace period elapsed,
// Service.advanceNoQueueItem would escalate with "no release was found" even
// though a release could well have been downloading; the fetch failure just
// couldn't say so. CheckPendingOutcome must now propagate the error so the
// caller's own error path (reschedule and retry, advance no state) runs
// instead.
func TestCheckPendingOutcome_QueueFetchFails_PropagatesError(t *testing.T) {
	t.Parallel()
	srv, rec := newArrStatusFixtureServer(t)
	defer srv.Close()
	rec.SetQueueFail(true)

	a := agent.New(
		nil,
		"",
		&agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")},
		nil,
		nil,
		slog.New(slog.DiscardHandler),
	)
	obs, err := a.CheckPendingOutcome(context.Background(), &db.PendingOutcome{
		MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9,
	})
	if err == nil {
		t.Fatal("expected an error when the queue fetch fails, got nil")
	}
	if obs != nil {
		t.Errorf("expected a nil observation alongside the error, got %+v", obs)
	}
}

// TestDispatchRestartDecypharr_WaitsForReady mirrors the Jellyfin test above
// for decypharr, whose readiness probe is RepairStatus rather than a
// dedicated ping endpoint.
func TestDispatchRestartDecypharr_WaitsForReady(t *testing.T) {
	t.Parallel()
	var statusCalls atomic.Int32
	decypharr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/repair/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if statusCalls.Add(1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"enabled":true}`))
	}))
	defer decypharr.Close()

	mediaAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/restart/decypharr" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mediaAgent.Close()

	disp := &agent.Dispatcher{
		Decypharr:  client.NewDecypharr(decypharr.URL, "token"),
		MediaAgent: client.NewMediaAgent(mediaAgent.URL, "key"),
	}
	result, err := disp.Call(context.Background(), "restart_decypharr", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if statusCalls.Load() != 2 {
		t.Errorf("RepairStatus called %d times, want 2 (waited for readiness)", statusCalls.Load())
	}
	if m, ok := result.(map[string]string); !ok || m["status"] != "restarted" {
		t.Errorf("result = %+v, want status=restarted", result)
	}
}
