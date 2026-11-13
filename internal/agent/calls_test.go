package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
)

const actMarker = "ACT"

type callServer struct {
	mu     sync.Mutex
	events []string

	failSeries     atomic.Int32
	emptySeries    bool
	emptyMovies    bool
	episodeHasFile bool
	scanRunning    bool
	scanStatusCode int
	pingStatus     int
	repairRunCode  int
	onRepairRun    func()
	onRestart      func()
	onCommand      func()
	onPing         func()
}

func (s *callServer) note(ev string) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *callServer) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *callServer) routeHook(path string) {
	switch {
	case strings.HasPrefix(path, "/restart/") && s.onRestart != nil:
		s.onRestart()
	case path == "/api/v3/command" && s.onCommand != nil:
		s.onCommand()
	}
}

func (s *callServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.note(r.Method + " " + r.URL.Path)
	s.routeHook(r.URL.Path)
	switch r.URL.Path {
	case "/api/repair/run":
		if s.onRepairRun != nil {
			s.onRepairRun()
		}
		if s.repairRunCode != 0 {
			w.WriteHeader(s.repairRunCode)
			return
		}
		writeJSON(w, map[string]string{"run_id": "r1"})
	case "/System/Ping":
		if s.onPing != nil {
			s.onPing()
		}
		if s.pingStatus != 0 {
			w.WriteHeader(s.pingStatus)
		}
	case "/ScheduledTasks":
		if s.scanStatusCode != 0 {
			w.WriteHeader(s.scanStatusCode)
			return
		}
		state := "Idle"
		if s.scanRunning {
			state = "Running"
		}
		writeJSON(w, []map[string]any{{"Key": "RefreshLibrary", "State": state}})
	case "/api/v3/series":
		if s.failSeries.Add(-1) >= 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if s.emptySeries {
			writeJSON(w, []client.Series{})
			return
		}
		writeJSON(w, []client.Series{{ID: 42, Title: "Rick and Morty"}})
	case "/api/v3/movie":
		if s.emptyMovies {
			writeJSON(w, []client.Movie{})
			return
		}
		writeJSON(w, []client.Movie{{ID: 5, Title: "Arrival", HasFile: false}})
	case "/api/v3/episode":
		writeJSON(w, []client.Episode{
			{ID: 901, SeriesID: 42, SeasonNumber: 9, EpisodeNumber: 7, EpisodeFileID: 7001, HasFile: s.episodeHasFile},
			{ID: 903, SeriesID: 42, SeasonNumber: 9, EpisodeNumber: 9, HasFile: false},
		})
	case "/api/v3/episodefile":
		writeJSON(w, []client.EpisodeFile{{ID: 7001, SeriesID: 42, Path: "/mnt/x/e07.mkv", Size: 1}})
	case "/api/episodes":
		s.writeBazarrEpisode(w)
	case "/api/episodes/subtitles":
		w.WriteHeader(http.StatusNoContent)
	case "/api/v3/command":
		writeJSON(w, map[string]int{"id": 1})
	default:
		writeJSON(w, map[string]any{})
	}
}

func newCallServer(t *testing.T) (*callServer, *httptest.Server) {
	t.Helper()
	s := &callServer{}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

func fullDispatcher(url string) *agent.Dispatcher {
	return &agent.Dispatcher{
		Decypharr:  client.NewDecypharr(url, "tok"),
		Jellyfin:   client.NewJellyfin(url, "key"),
		Sonarr:     client.NewArr(url, "key"),
		Radarr:     client.NewArr(url, "key"),
		MediaAgent: client.NewMediaAgent(url, "key"),
		Bazarr:     client.NewBazarr(url, "key"),
	}
}

type seqGate struct {
	srv *callServer
}

func (g seqGate) Claim(context.Context) error { return nil }

func (g seqGate) Act(context.Context, string, map[string]any) (agent.Verdict, error) {
	g.srv.note(actMarker)
	return agent.GateAllowed, nil
}

func (g seqGate) Gen() int64 { return 0 }

func (g seqGate) ActionLogged(context.Context) {}

func (g seqGate) Live(context.Context) agent.Verdict { return agent.GateAllowed }

func (g seqGate) Calls() []agent.CallRecord { return nil }

func (g seqGate) Record(agent.CallRecord) {}

func (seqGate) HoldStale(int) {}

type fixedGate struct {
	verdict agent.Verdict
	err     error
	acts    atomic.Int32
}

func (g *fixedGate) Claim(context.Context) error { return nil }

func (g *fixedGate) Act(context.Context, string, map[string]any) (agent.Verdict, error) {
	g.acts.Add(1)
	return g.verdict, g.err
}

func (g *fixedGate) Gen() int64 { return 0 }

func (g *fixedGate) ActionLogged(context.Context) {}

func (g *fixedGate) Live(context.Context) agent.Verdict { return agent.GateAllowed }

func (g *fixedGate) Calls() []agent.CallRecord { return nil }

func (g *fixedGate) Record(agent.CallRecord) {}

func (*fixedGate) HoldStale(int) {}

func isReadOnlyPOST(method, path string) bool {
	return method == http.MethodPost && strings.HasPrefix(path, "/Items/") && strings.HasSuffix(path, "/PlaybackInfo")
}

func splitEvent(ev string) (string, string) {
	method, path, _ := strings.Cut(ev, " ")
	return method, path
}

func checkActOrder(t *testing.T, events []string) {
	t.Helper()
	actAt, acts, effects := -1, 0, 0
	for i, ev := range events {
		if ev == actMarker {
			actAt = i
			acts++
			continue
		}
		method, path := splitEvent(ev)
		if method == http.MethodGet || isReadOnlyPOST(method, path) {
			continue
		}
		effects++
		if actAt < 0 {
			t.Errorf("side effect %q ran before Act: %v", ev, events)
		}
	}
	if acts != 1 {
		t.Errorf("Act ran %d times, want once: %v", acts, events)
	}
	if effects == 0 {
		t.Errorf("no side effect request was made: %v", events)
	}
}

func TestWriteTools_ActBeforeSideEffect(t *testing.T) {
	t.Parallel()
	tv := map[string]any{"media_type": "tv", "title": "Rick and Morty"}
	withKeys := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		maps.Copy(out, tv)
		maps.Copy(out, extra)
		return out
	}
	cases := []struct {
		label, tool string
		args        map[string]any
	}{
		{"refresh", "refresh_decypharr_links", map[string]any{}},
		{"sweep", "decypharr_repair_sweep", map[string]any{}},
		{"cleanup", "decypharr_cache_cleanup", map[string]any{}},
		{"scan", "jellyfin_library_scan", map[string]any{}},
		{"restart decypharr", "restart_decypharr", map[string]any{}},
		{"restart jellyfin", "restart_jellyfin", map[string]any{}},
		{"recheck", "decypharr_recheck", map[string]any{"name": "x"}},
		{"sonarr rescan", "sonarr_rescan", map[string]any{"title": "Rick and Morty"}},
		{"radarr rescan", "radarr_rescan", map[string]any{"title": "Arrival"}},
		{"clear cache", "clear_jellyfin_cache", map[string]any{"item_id": "abc"}},
		{"search episode", "arr_search_missing", withKeys(map[string]any{"season": 9, "episode": 9})},
		{"search season", "arr_search_missing", withKeys(map[string]any{"season": 9})},
		{"search series", "arr_search_missing", withKeys(nil)},
		{"search movie", "arr_search_missing", map[string]any{"media_type": "movie", "title": "Arrival"}},
		{
			"bazarr search",
			"bazarr_search_subtitles",
			withKeys(map[string]any{"season": 9, "episode": 9, "language": "en"}),
		},
	}
	covered := map[string]bool{"bazarr_resync_subtitle": true}
	for _, tc := range cases {
		covered[tc.tool] = true
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			s, srv := newCallServer(t)
			d := fullDispatcher(srv.URL)
			if _, err := d.CallGated(t.Context(), agent.Scope{}, seqGate{srv: s}, tc.tool, tc.args); err != nil {
				t.Fatalf("%s: %v", tc.tool, err)
			}
			checkActOrder(t, s.log())
		})
	}
	for _, spec := range agent.ToolRegistry() {
		if spec.Risk == agent.RiskWrite && !covered[spec.Name] {
			t.Errorf("write tool %s has no act-ordering case", spec.Name)
		}
	}
}

func episodeArgs(episode int) map[string]any {
	return map[string]any{"media_type": "tv", "title": "Rick and Morty", "season": 9, "episode": episode}
}

func countBeforeAct(events []string, target string) int {
	n := 0
	for _, ev := range events {
		if ev == actMarker {
			break
		}
		if ev == target {
			n++
		}
	}
	return n
}

func TestArrSearch_ActAfterEpisodeLookup(t *testing.T) {
	t.Parallel()
	t.Run("known episode", func(t *testing.T) {
		t.Parallel()
		s, srv := newCallServer(t)
		d := fullDispatcher(srv.URL)
		gate := seqGate{srv: s}
		if _, err := d.CallGated(t.Context(), agent.Scope{}, gate, "arr_search_missing", episodeArgs(9)); err != nil {
			t.Fatal(err)
		}
		const want = 2
		if got := countBeforeAct(s.log(), "GET /api/v3/episode"); got != want {
			t.Errorf("%d episode GETs before Act, want %d (status read, then the id lookup): %v", got, want, s.log())
		}
	})
	t.Run("unknown episode", func(t *testing.T) {
		t.Parallel()
		s, srv := newCallServer(t)
		d := fullDispatcher(srv.URL)
		gate := seqGate{srv: s}
		_, err := d.CallGated(t.Context(), agent.Scope{}, gate, "arr_search_missing", episodeArgs(99))
		if err == nil || err.Error() != "episode not found in sonarr" {
			t.Fatalf("err = %v, want the refusal %q", err, "episode not found in sonarr")
		}
		if slices.Contains(s.log(), actMarker) || slices.Contains(s.log(), "POST /api/v3/command") {
			t.Errorf("Act or a search ran for an unknown episode: %v", s.log())
		}
	})
}

type rigOpts struct {
	journalDB *db.DB
	log       *slog.Logger
	disp      func(*agent.Dispatcher)
	onLLM     func()
}

func newDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "rig-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func newRig(t *testing.T, srvURL string, responses []string, opts rigOpts) (*agent.Agent, *db.DB, *db.Incident) {
	t.Helper()
	llm := &scriptedLLM{responses: responses}
	var once sync.Once
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if opts.onLLM != nil {
			once.Do(opts.onLLM)
		}
		llm.ServeHTTP(w, r)
	}))
	t.Cleanup(llmSrv.Close)
	database := newDB(t)
	jdb := database
	if opts.journalDB != nil {
		jdb = opts.journalDB
	}
	cfg := openai.DefaultConfig("stub-key")
	cfg.BaseURL = llmSrv.URL
	disp := fullDispatcher(srvURL)
	if opts.disp != nil {
		opts.disp(disp)
	}
	log := opts.log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ag := agent.New(openai.NewClientWithConfig(cfg), "stub", disp, database, journal.New(jdb), log)
	return ag, database, seedRunIncident(t, database)
}

func script(tool, args string) []string {
	return []string{toolCallResponse(tool, args), toolCallResponse("complete_diagnosis", diagnosisArgs)}
}

func listActions(t *testing.T, database *db.DB, id string) []db.ActionLog {
	t.Helper()
	acts, err := database.ListActions(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]db.ActionLog, 0, len(acts))
	for _, a := range acts {
		out = append(out, *a)
	}
	return out
}

func actionCount(t *testing.T, database *db.DB, id string) int {
	t.Helper()
	snap, err := database.GetIncidentSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return snap.ActionCount
}

func TestA1_4_RefusedCallNotCountedAndLogged(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.episodeHasFile = true
	args := `{"media_type":"tv","title":"Rick and Morty","season":9,"episode":7}`
	ag, database, inc := newRig(t, srv.URL, script("arr_search_missing", args), rigOpts{})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 0 {
		t.Fatalf("Act ran for a refused call: %v", got)
	}
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionRefused {
		t.Fatalf("actions = %+v, want one refused row", acts)
	}
	if acts[0].Error != agent.ErrArrTargetHasFile.Error() {
		t.Errorf("row error = %q, want %q", acts[0].Error, agent.ErrArrTargetHasFile.Error())
	}
	params, _ := acts[0].Params.(map[string]any)
	if params["title"] != "Rick and Morty" || params["season"] != float64(9) {
		t.Errorf("params = %v, want the identity subset of the raw args", params)
	}
	if _, has := params["media_type"]; has {
		t.Errorf("params = %v, want only identity keys", params)
	}
	if n := actionCount(t, database, inc.ID); n != 0 {
		t.Errorf("action_count = %d, want 0", n)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], agent.ErrArrTargetHasFile.Error()[:30]) {
		t.Errorf("reply = %v, want it to carry the refusal", replies)
	}
}

type refusalCase struct {
	label, tool, args, want string
	setup                   func(*callServer)
	disp                    func(*agent.Dispatcher)
}

func assertRefused(t *testing.T, tc refusalCase) {
	t.Helper()
	s, srv := newCallServer(t)
	if tc.setup != nil {
		tc.setup(s)
	}
	ag, database, inc := newRig(t, srv.URL, script(tc.tool, tc.args), rigOpts{disp: tc.disp})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 0 {
		t.Errorf("Act ran for a refusal: %v", got)
	}
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionRefused || acts[0].Error != tc.want {
		t.Fatalf("actions = %+v, want one refused row with error %q", acts, tc.want)
	}
	if n := actionCount(t, database, inc.ID); n != 0 {
		t.Errorf("action_count = %d, want 0", n)
	}
	wantReply := strings.Replace(tc.want, "a library scan is already running", "already_running", 1)
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], wantReply) {
		t.Errorf("reply = %v, want it to carry %q", replies, wantReply)
	}
}

func TestA1_4_RefusalsTable(t *testing.T) {
	t.Parallel()
	cases := []refusalCase{
		{"series not found", "sonarr_rescan", `{"title":"Nope"}`, "series not found in sonarr",
			func(s *callServer) { s.emptySeries = true }, nil},
		{"movie not found", "radarr_rescan", `{"title":"Nope"}`, "movie not found in radarr",
			func(s *callServer) { s.emptyMovies = true }, nil},
		{"episode not found", "arr_search_missing",
			`{"media_type":"tv","title":"Rick and Morty","season":9,"episode":99}`,
			"episode not found in sonarr", nil, nil},
		{"no name", "decypharr_recheck", `{}`, "name is required", nil, nil},
		{"media agent unconfigured", "restart_jellyfin", `{}`, "media-agent not configured", nil,
			func(d *agent.Dispatcher) { d.MediaAgent = nil }},
		{"scan already running", "jellyfin_library_scan", `{}`, "a library scan is already running",
			func(s *callServer) { s.scanRunning = true }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			assertRefused(t, tc)
		})
	}
}

func TestA1_4_TransientPreconditionErrorNotRefused(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	args := `{"media_type":"tv","title":"Rick and Morty","season":9,"episode":9}`
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", args),
		toolCallResponse("arr_search_missing", args),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{onLLM: func() { s.failSeries.Store(1) }})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) < 2 || !strings.Contains(replies[0], "not run: ") {
		t.Fatalf("first reply = %v, want \"not run: ...\"", replies)
	}
	if got := g.acted(); len(got) != 1 {
		t.Errorf("Act ran %d times, want only for the repeat: %v", len(got), got)
	}
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionApplied {
		t.Errorf("actions = %+v, want only the repeat's applied row", acts)
	}
	if n := actionCount(t, database, inc.ID); n != 1 {
		t.Errorf("action_count = %d, want 1", n)
	}
}

func TestOutcome_GateErrorIsNotRun(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, script("refresh_decypharr_links", `{}`), rigOpts{})
	g := &fixedGate{err: errors.New("db is down")}
	exit := ag.Run(context.Background(), inc, nil, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], "not run: could not record the action") {
		t.Errorf("reply = %v, want \"not run: could not record the action\"", replies)
	}
	if acts := listActions(t, database, inc.ID); len(acts) != 0 {
		t.Errorf("actions = %+v, want none", acts)
	}
}

func TestA6_6_RestartNotReadyLoggedAppliedDisruptive(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.pingStatus = http.StatusServiceUnavailable
	ag, database, inc := newRig(t, srv.URL, script("restart_jellyfin", `{}`), rigOpts{})
	ag.SetReadinessForTest(30*time.Millisecond, 10*time.Millisecond)
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionApplied {
		t.Fatalf("actions = %+v, want one applied row", acts)
	}
	if !strings.Contains(acts[0].Error, "did not become ready") {
		t.Errorf("row error = %q, want the readiness error kept", acts[0].Error)
	}
	disp, err := journal.New(database).LastDisruption(context.Background())
	if err != nil || disp.Action != "restart_jellyfin" {
		t.Errorf("LastDisruption = %+v, %v, want restart_jellyfin", disp, err)
	}
	if !ag.ActionAlreadyAppliedForTest(context.Background(), inc.ID, "restart_jellyfin", `{"reason":"x"}`) {
		t.Error("a later restart_jellyfin is not blocked by actionAlreadyApplied")
	}
}

func TestA6_6_RestartCutBySigtermStillApplied(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, srv := newCallServer(t)
	s.pingStatus = http.StatusServiceUnavailable
	s.onPing = cancel
	ag, database, inc := newRig(t, srv.URL, script("restart_jellyfin", `{}`), rigOpts{})
	ag.Run(ctx, inc, nil, newRecGate(database, inc))
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionApplied {
		t.Fatalf("actions = %+v, want one applied row", acts)
	}
	disp, err := journal.New(database).LastDisruption(context.Background())
	if err != nil || disp.Action != "restart_jellyfin" {
		t.Errorf("LastDisruption = %+v, %v, want restart_jellyfin", disp, err)
	}
}

func TestA6_7_ActionLoggedAfterCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, srv := newCallServer(t)
	s.onRepairRun = cancel
	ag, database, inc := newRig(t, srv.URL, script("refresh_decypharr_links", `{}`), rigOpts{})
	ag.Run(ctx, inc, nil, newRecGate(database, inc))
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Action != "refresh_decypharr_links" {
		t.Fatalf("actions = %+v, want the refresh recorded after the cancel", acts)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestA6_7_LostRecordLogsError(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	broken := newDB(t)
	broken.Close()
	var out lockedBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ag, database, inc := newRig(t, srv.URL, script("clear_jellyfin_cache", `{}`),
		rigOpts{journalDB: broken, log: log})
	exit := ag.Run(context.Background(), inc, nil, newRecGate(database, inc))
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	var lost []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "action record lost") {
			lost = append(lost, line)
		}
	}
	if len(lost) != 1 {
		t.Fatalf("%d \"action record lost\" lines, want 1:\n%s", len(lost), out.String())
	}
	for _, want := range []string{"level=ERROR", "tool=clear_jellyfin_cache", "outcome=applied"} {
		if !strings.Contains(lost[0], want) {
			t.Errorf("line %q missing %q", lost[0], want)
		}
	}
}

func TestOutcome_FailedAfterActCountedAndLogged(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.repairRunCode = http.StatusInternalServerError
	ag, database, inc := newRig(t, srv.URL, script("refresh_decypharr_links", `{}`), rigOpts{})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	acts := listActions(t, database, inc.ID)
	if len(acts) != 1 || acts[0].Status != db.ActionFailed || acts[0].Error == "" {
		t.Fatalf("actions = %+v, want one failed row with its error", acts)
	}
	if n := actionCount(t, database, inc.ID); n != 1 {
		t.Errorf("action_count = %d, want 1", n)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], "status 500") {
		t.Errorf("reply = %v, want the failure", replies)
	}
}

func TestOutcome_GateDeniedNotRecorded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		verdict   agent.Verdict
		wantState agent.RunState
	}{
		{agent.GateTripped, agent.RunBudgetTripped},
		{agent.GateDiagnosisOnly, agent.RunConcluded},
		{agent.GatePaused, agent.RunPaused},
		{agent.GateSuperseded, agent.RunSuperseded},
	}
	for _, tc := range cases {
		t.Run(string(tc.verdict), func(t *testing.T) {
			t.Parallel()
			_, srv := newCallServer(t)
			ag, database, inc := newRig(t, srv.URL, script("refresh_decypharr_links", `{}`), rigOpts{})
			g := &fixedGate{verdict: tc.verdict}
			exit := ag.Run(context.Background(), inc, nil, g)
			if exit.State != tc.wantState {
				t.Errorf("exit %s, want %s", exit.State, tc.wantState)
			}
			if acts := listActions(t, database, inc.ID); len(acts) != 0 {
				t.Errorf("actions = %+v, want none", acts)
			}
			replies := toolReplies(exit.Conversation)
			if len(replies) == 0 || !strings.Contains(replies[0], "not run: ") {
				t.Errorf("reply = %v, want \"not run: ...\"", replies)
			}
		})
	}
}

func TestAttemptHistory_ShowsRefused(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusReopened, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T"}
	lifecycletest.Incident(t, database, inc)
	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "sonarr_rescan", Params: map[string]any{"title": "T"},
		TriggeredBy: "agent", Status: db.ActionRefused, Error: "series not found in sonarr",
	}); err != nil {
		t.Fatal(err)
	}
	a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))
	got := a.AttemptHistoryForTest(ctx, inc.ID)
	for _, want := range []string{"each marked applied, failed, refused or outcome unknown (interrupted)", "refused", "series not found in sonarr"} {
		if !strings.Contains(got, want) {
			t.Errorf("attempt history missing %q; got:\n%s", want, got)
		}
	}
}

func TestBudgetTrip_FindingSaysTried(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("decypharr_repair_sweep", `{}`),
		toolCallResponse("decypharr_cache_cleanup", `{}`),
		toolCallResponse("decypharr_recheck", `{"name":"something"}`),
	}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunBudgetTripped || exit.Finding == nil {
		t.Fatalf("exit %s finding %+v", exit.State, exit.Finding)
	}
	const want = "agent tried 3 autonomous actions without confirming a fix"
	if exit.Finding.PrimaryReason != want {
		t.Errorf("PrimaryReason = %q, want %q", exit.Finding.PrimaryReason, want)
	}
}

func cancelThenLinger(cancel context.CancelFunc) func() {
	return func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
	}
}

func TestReview2_SideEffectSurvivesRunCancel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool, args string
		hook       func(*callServer, func())
	}{
		{"restart_jellyfin", `{}`, func(s *callServer, c func()) { s.onRestart = c }},
		{"restart_decypharr", `{}`, func(s *callServer, c func()) { s.onRestart = c }},
		{"arr_search_missing", searchEpisode7, func(s *callServer, c func()) { s.onCommand = c }},
		{"refresh_decypharr_links", `{}`, func(s *callServer, c func()) { s.onRepairRun = c }},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, srv := newCallServer(t)
			tc.hook(s, cancelThenLinger(cancel))
			ag, database, inc := newRig(t, srv.URL, script(tc.tool, tc.args), rigOpts{})
			ag.Run(ctx, inc, nil, newRecGate(database, inc))
			acts := listActions(t, database, inc.ID)
			if len(acts) != 1 || acts[0].Status != db.ActionApplied {
				t.Fatalf("actions = %+v, want one applied row", acts)
			}
		})
	}
}

func hangingRepairRun(t *testing.T) *httptest.Server {
	t.Helper()
	s := &callServer{}
	s.onRepairRun = func() { time.Sleep(time.Second) }
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func TestReview2_BoundCutIsInterrupted(t *testing.T) {
	t.Parallel()
	srv := hangingRepairRun(t)
	var out lockedBuffer
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ag, database, inc := newRig(t, srv.URL, script("refresh_decypharr_links", `{}`), rigOpts{log: log})
	ag.SetSideEffectBoundForTest(150 * time.Millisecond)
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if acts := listActions(t, database, inc.ID); len(acts) != 0 {
		t.Errorf("actions = %+v, want no row", acts)
	}
	calls := g.Calls()
	if len(calls) != 1 || calls[0].Status != agent.OutcomeInterrupted || !calls[0].Counted {
		t.Errorf("gate records = %+v, want one counted interrupted", calls)
	}
	var warns []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "action outcome unknown") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "level=WARN") ||
		!strings.Contains(warns[0], "tool=refresh_decypharr_links") {
		t.Errorf("outcome-unknown lines = %v, want one WARN naming the tool", warns)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], "unknown") {
		t.Errorf("reply = %v, want the outcome unknown", replies)
	}
}

func (s *callServer) writeBazarrEpisode(w http.ResponseWriter) {
	subs := []any{}
	if slices.Contains(s.log(), "PATCH /api/episodes/subtitles") {
		subs = append(subs, map[string]any{"code2": "en", "path": "/x/e09.en.srt", "file_size": 1})
	}
	writeJSON(w, map[string]any{"data": []map[string]any{{"subtitles": subs}}})
}
