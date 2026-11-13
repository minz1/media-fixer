package agent_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/journal"
)

const (
	syncedRow = `{"action":5,"subtitles_path":"/tv/Show.S01E02.en.srt",` +
		`"description":"English subtitles synchronization ended with an offset of 2.31 seconds and a framerate scale factor of 1."}`
	enSrt   = `{"code2":"en","path":"/tv/Show.S01E02.en.srt","file_size":100}`
	enHISrt = `{"code2":"en","path":"/tv/Show.S01E02.en.hi.srt","hi":true,"file_size":100}`
)

type resyncFake struct {
	mu        sync.Mutex
	events    []string
	patches   []url.Values
	history   int
	subs      string
	patchCode int
	fileCode  int
	files     string
	epFileID  int
	movies    string
	hist      func(read int) (int, string)
}

func (f *resyncFake) log(e string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
}

func (f *resyncFake) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.patches)
}

func (f *resyncFake) eventList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *resyncFake) bazarr(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPatch && r.URL.Path == "/api/subtitles":
		f.patches = append(f.patches, r.URL.Query())
		f.events = append(f.events, "patch")
		w.WriteHeader(f.patchCode)
	case r.URL.Path == "/api/episodes" || r.URL.Path == "/api/movies":
		_, _ = w.Write([]byte(subsBody(f.subs)))
	case r.URL.Path == "/api/episodes/history" || r.URL.Path == "/api/movies/history":
		f.history++
		status, body := f.hist(f.history)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	default:
		http.NotFound(w, r)
	}
}

func (f *resyncFake) arr(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/v3/series":
		_, _ = w.Write([]byte(`[{"id":1,"title":"Show"}]`))
	case "/api/v3/episode":
		_, _ = w.Write([]byte(`[{"id":42,"seriesId":1,"seasonNumber":1,"episodeNumber":2,"episodeFileId":` +
			strconv.Itoa(f.epFileID) + `}]`))
	case "/api/v3/episodefile", "/api/v3/moviefile":
		w.WriteHeader(f.fileCode)
		_, _ = w.Write([]byte(f.files))
	case "/api/v3/movie":
		_, _ = w.Write([]byte(f.movies))
	default:
		http.NotFound(w, r)
	}
}

func historyBody(rows ...string) string { return `{"data":[` + strings.Join(rows, ",") + `]}` }

func historyGrowsAt(n int) func(int) (int, string) {
	return func(read int) (int, string) {
		if read >= n {
			return http.StatusOK, historyBody(syncedRow)
		}
		return http.StatusOK, historyBody()
	}
}

func newResyncFake() *resyncFake {
	return &resyncFake{
		subs: enSrt, patchCode: http.StatusNoContent, fileCode: http.StatusOK,
		files: `[{"id":7,"seriesId":1,"size":1000}]`, epFileID: 7,
		movies: `[{"id":9,"title":"Film"}]`, hist: historyGrowsAt(2),
	}
}

type fakeAttempts struct{ list []journal.Attempt }

func (f *fakeAttempts) AttemptsSince(_ context.Context, tool string, since time.Time) ([]journal.Attempt, error) {
	var out []journal.Attempt
	for _, a := range f.list {
		if a.Tool == tool && !a.At.Before(since) {
			out = append(out, a)
		}
	}
	return out, nil
}

type holdGate struct {
	f     *resyncFake
	holds []int
}

func (g *holdGate) Claim(context.Context) error { return nil }
func (g *holdGate) Act(context.Context, string, map[string]any) (agent.Verdict, error) {
	g.f.log("act")
	return agent.GateAllowed, nil
}
func (g *holdGate) Gen() int64 { return 1 }
func (g *holdGate) HoldStale(minutes int) {
	g.holds = append(g.holds, minutes)
	g.f.log("hold")
}
func (g *holdGate) ActionLogged(context.Context)       {}
func (g *holdGate) Live(context.Context) agent.Verdict { return agent.GateAllowed }
func (g *holdGate) Calls() []agent.CallRecord          { return nil }
func (g *holdGate) Record(agent.CallRecord)            {}

func newResyncDisp(t *testing.T, f *resyncFake, attempts agent.AttemptHistory) *agent.Dispatcher {
	t.Helper()
	bz := httptest.NewServer(http.HandlerFunc(f.bazarr))
	t.Cleanup(bz.Close)
	arr := httptest.NewServer(http.HandlerFunc(f.arr))
	t.Cleanup(arr.Close)
	return &agent.Dispatcher{
		Sonarr: client.NewArr(arr.URL, "k"), Radarr: client.NewArr(arr.URL, "k"),
		Bazarr: client.NewBazarr(bz.URL, "k"), BazarrWaits: &agent.BazarrWaits{},
		Attempts: attempts, ResyncMaxBytes: 20 << 30,
	}
}

func resyncArgs() map[string]any {
	return map[string]any{"media_type": "tv", "title": "Show", "season": 1, "episode": 2, "language": "en"}
}

func runResync(
	ctx context.Context, t *testing.T, disp *agent.Dispatcher, g agent.Gate, what string, args map[string]any,
	total time.Duration,
) (*agent.BazarrResyncResult, error) {
	t.Helper()
	out, err := disp.CallScopedPolledForTest(ctx, what, g, 5*time.Millisecond, total, "bazarr_resync_subtitle", args)
	if err != nil {
		return nil, err
	}
	res, ok := out.(*agent.BazarrResyncResult)
	if !ok {
		t.Fatalf("result type %T", out)
	}
	return res, nil
}

func TestRS4_SyncsOneSidecar(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	g := &holdGate{f: f}
	disp := newResyncDisp(t, f, &fakeAttempts{})
	res, err := runResync(t.Context(), t, disp, g, agent.WhatSubtitles, resyncArgs(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Synced || res.Offset != "+2.3 s" || res.Language != "en" || res.Path != "/tv/Show.S01E02.en.srt" ||
		res.Summary != "Resynced en subtitles to the audio (offset +2.3 s)" {
		t.Fatalf("res %+v", res)
	}
	if res.Title != "Show" || res.Season != 1 || res.Episode != 2 || res.Movie {
		t.Errorf("target %+v", res)
	}
	if ev := f.eventList(); !slices.Equal(ev, []string{"hold", "act", "patch"}) {
		t.Errorf("events %v, want hold, act, then one patch", ev)
	}
	q := f.patches[0]
	if q.Get("action") != "sync" || q.Get("path") != "/tv/Show.S01E02.en.srt" || q.Get("type") != "episode" ||
		q.Get("id") != "42" || q.Get("hi") != "false" {
		t.Errorf("patch %v", q)
	}
}

func TestRS4_PicksNonHI(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	f.subs = enHISrt + "," + enSrt
	disp := newResyncDisp(t, f, &fakeAttempts{})
	res, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(), 2*time.Second)
	if err != nil || res.Path != "/tv/Show.S01E02.en.srt" || f.patches[0].Get("hi") != "false" {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func checkRefusal(t *testing.T, name, what, lang string, setup func(*resyncFake, *agent.Dispatcher)) {
	t.Helper()
	f := newResyncFake()
	g := &holdGate{f: f}
	disp := newResyncDisp(t, f, &fakeAttempts{})
	args := resyncArgs()
	if name == "two movie files" {
		args = map[string]any{"media_type": "movie", "title": "Film", "language": "en"}
	}
	if lang != "" {
		args["language"] = lang
	}
	scope := agent.WhatSubtitles
	if what != "" {
		scope = what
	}
	if setup != nil {
		setup(f, disp)
	}
	_, err := runResync(t.Context(), t, disp, g, scope, args, 50*time.Millisecond)
	if err == nil {
		t.Fatal("not refused")
	}
	if lang == "zz" && !errors.Is(err, agent.ErrUnknownLanguage) {
		t.Errorf("err = %v, want ErrUnknownLanguage", err)
	}
	if f.patchCount() != 0 || len(g.holds) != 0 {
		t.Errorf("patches %d holds %v", f.patchCount(), g.holds)
	}
	if (lang == "zz" || what != "") && f.history != 0 {
		t.Errorf("bazarr history read before the refusal")
	}
}

func TestRS4_Refusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		what  string
		lang  string
		setup func(*resyncFake, *agent.Dispatcher)
	}{
		{name: "unknown language", lang: "zz"},
		{name: "playback scope", what: "cant_play"},
		{name: "embedded only", setup: func(f *resyncFake, _ *agent.Dispatcher) { f.subs = `{"code2":"en","path":null}` }},
		{name: "forced only", setup: func(f *resyncFake, _ *agent.Dispatcher) {
			f.subs = `{"code2":"en","path":"/tv/Show.S01E02.en.forced.srt","forced":true}`
		}},
		{name: "sub only", setup: func(f *resyncFake, _ *agent.Dispatcher) {
			f.subs = `{"code2":"en","path":"/tv/Show.S01E02.en.sub"}`
		}},
		{name: "two non-HI", setup: func(f *resyncFake, _ *agent.Dispatcher) {
			f.subs = enSrt + `,{"code2":"en","path":"/tv/Show.S01E02.en.ass"}`
		}},
		{name: "other language only", setup: func(f *resyncFake, _ *agent.Dispatcher) {
			f.subs = `{"code2":"fr","path":"/tv/Show.S01E02.fr.srt"}`
		}},
		{name: "over the cap", setup: func(_ *resyncFake, d *agent.Dispatcher) { d.ResyncMaxBytes = 999 }},
		{name: "two movie files", setup: func(f *resyncFake, _ *agent.Dispatcher) {
			f.files = `[{"id":1,"movieId":9,"size":10},{"id":2,"movieId":9,"size":10}]`
		}},
		{name: "attempts nil", setup: func(_ *resyncFake, d *agent.Dispatcher) { d.Attempts = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checkRefusal(t, tc.name, tc.what, tc.lang, tc.setup)
		})
	}
}

func TestRS4_UnknownSizeRefused(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*resyncFake){
		"file lookup 500": func(f *resyncFake) { f.fileCode = http.StatusInternalServerError },
		"no episode file": func(f *resyncFake) { f.epFileID = 0 },
		"file id missing": func(f *resyncFake) { f.files = `[{"id":8,"seriesId":1,"size":1000}]` },
		"size zero":       func(f *resyncFake) { f.files = `[{"id":7,"seriesId":1,"size":0}]` },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newResyncFake()
			setup(f)
			disp := newResyncDisp(t, f, &fakeAttempts{})
			if _, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(),
				50*time.Millisecond); err == nil || f.patchCount() != 0 {
				t.Fatalf("err %v patches %d", err, f.patchCount())
			}
		})
	}
}

func TestRS4_GuardCountsEveryAttempt(t *testing.T) {
	t.Parallel()
	path := "/tv/Show.S01E02.en.srt"
	for _, tc := range []struct {
		name string
		at   time.Duration
		path string
		sent bool
	}{
		{"same path 3h ago", 3 * time.Hour, path, false},
		{"other path", 3 * time.Hour, "/tv/Other.en.srt", true},
		{"same path 25h ago", 25 * time.Hour, path, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newResyncFake()
			hist := &fakeAttempts{list: []journal.Attempt{{
				Tool: "bazarr_resync_subtitle", Params: map[string]any{"path": tc.path}, At: time.Now().Add(-tc.at),
			}}}
			disp := newResyncDisp(t, f, hist)
			_, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(), 2*time.Second)
			if sent := f.patchCount() == 1; sent != tc.sent || (err == nil) != tc.sent {
				t.Fatalf("sent %v err %v, want sent %v", sent, err, tc.sent)
			}
		})
	}
}

func TestRS4_OldRowDoesNotCount(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	f.hist = func(int) (int, string) { return http.StatusOK, historyBody(syncedRow) }
	disp := newResyncDisp(t, f, &fakeAttempts{})
	res, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(), 60*time.Millisecond)
	if err != nil || res.Synced {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestRS4_NoRowTimesOut(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	f.hist = historyGrowsAt(1 << 30)
	disp := newResyncDisp(t, f, &fakeAttempts{})
	res, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(), 60*time.Millisecond)
	if err != nil || res.Synced || f.patchCount() != 1 {
		t.Fatalf("res %+v err %v patches %d", res, err, f.patchCount())
	}
	if !strings.HasPrefix(res.Summary, "Bazarr did not report a finished sync of Show.S01E02.en.srt in") ||
		!strings.Contains(res.Summary, "Bazarr may still finish later and rewrite the file") {
		t.Errorf("summary %q", res.Summary)
	}
}

func TestRS4_HistoryReadErrorKeepsPolling(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	f.hist = func(read int) (int, string) {
		switch read {
		case 1:
			return http.StatusOK, historyBody()
		case 2:
			return http.StatusInternalServerError, ""
		}
		return http.StatusOK, historyBody(syncedRow)
	}
	disp := newResyncDisp(t, f, &fakeAttempts{})
	res, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(), 2*time.Second)
	if err != nil || !res.Synced {
		t.Fatalf("res %+v err %v", res, err)
	}
}

func TestRS4_PatchError(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	f.patchCode = http.StatusInternalServerError
	disp := newResyncDisp(t, f, &fakeAttempts{})
	_, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(), 2*time.Second)
	if err == nil || f.history != 1 {
		t.Fatalf("err %v history reads %d, want the baseline read only", err, f.history)
	}
}

func TestRS4_SupersedeEndsWait(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	f.hist = historyGrowsAt(1 << 30)
	disp := newResyncDisp(t, f, &fakeAttempts{})
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		_, err := runResync(ctx, t, disp, &holdGate{f: f}, agent.WhatSubtitles, resyncArgs(), time.Hour)
		errc <- err
	}()
	for f.patchCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) || f.patchCount() != 1 {
			t.Fatalf("err %v patches %d", err, f.patchCount())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not end on cancel")
	}
}

func TestRS4_Movie(t *testing.T) {
	t.Parallel()
	f := newResyncFake()
	f.files = `[{"id":1,"movieId":9,"size":10}]`
	f.subs = `{"code2":"en","path":"/tv/Show.S01E02.en.srt"}`
	disp := newResyncDisp(t, f, &fakeAttempts{})
	args := map[string]any{"media_type": "movie", "title": "Film", "language": "en"}
	res, err := runResync(t.Context(), t, disp, &holdGate{f: f}, agent.WhatSubtitles, args, 2*time.Second)
	if err != nil || !res.Synced || !res.Movie || res.Title != "Film" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if q := f.patches[0]; q.Get("type") != "movie" || q.Get("id") != "9" {
		t.Errorf("patch %v", q)
	}
}

func TestRS4_LastResync(t *testing.T) {
	t.Parallel()
	call := func(id, name string) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
			ID: id, Function: openai.FunctionCall{Name: name},
		}}}
	}
	reply := func(id, body string) openai.ChatCompletionMessage {
		return openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: body}
	}
	msgs := []openai.ChatCompletionMessage{
		call("a", "bazarr_resync_subtitle"), reply("a", `{"synced":true,"summary":"first","language":"en"}`),
		call("b", "bazarr_resync_subtitle"), reply("b", `{"error":"boom"}`),
		call("c", "bazarr_search_subtitles"), reply("c", `{"synced":false,"summary":"search"}`),
	}
	got := agent.LastBazarrResyncForTest(msgs)
	if got == nil || !got.Synced || got.Summary != "first" {
		t.Fatalf("last = %+v", got)
	}
	if agent.LastBazarrResyncForTest(nil) != nil {
		t.Error("no reply must give nil")
	}
}

func TestRS4_Registry(t *testing.T) {
	t.Parallel()
	spec, ok := agent.SpecByNameForTest("bazarr_resync_subtitle")
	if !ok || spec.Risk != agent.RiskWrite || agent.DisruptiveForTest(spec) {
		t.Fatalf("spec %v risk %v disruptive %v", ok, spec.Risk, agent.DisruptiveForTest(spec))
	}
	if tier, _ := agent.LiveTierOf("bazarr_resync_subtitle"); tier != agent.LiveTierWrite {
		t.Errorf("tier %q", tier)
	}
	search, _ := agent.SpecByNameForTest("bazarr_search_subtitles")
	for _, d := range []*agent.Dispatcher{
		{Bazarr: client.NewBazarr("http://b", "k")},
		{Sonarr: client.NewArr("http://s", "k")},
		{Bazarr: client.NewBazarr("http://b", "k"), Sonarr: client.NewArr("http://s", "k")},
	} {
		gotReason, gotMet := spec.Needs(d)
		wantReason, wantMet := search.Needs(d)
		if gotReason != wantReason || gotMet != wantMet {
			t.Errorf("needs = %q %v, want the search's %q %v", gotReason, gotMet, wantReason, wantMet)
		}
	}
}
