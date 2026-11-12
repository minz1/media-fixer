package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
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

const (
	oldSub = `{"code2":"en","path":"/tv/Show.en.srt","file_size":100}`
	newSub = `{"code2":"en","path":"/tv/Show.S01E02.en.srt","file_size":200}`
)

type bazarrFake struct {
	mu         sync.Mutex
	reads      int
	patches    int
	requests   atomic.Int64
	subs       func(read int) (int, string)
	candidates string
}

func subsBody(subs ...string) string {
	return `{"data":[{"subtitles":[` + strings.Join(subs, ",") + `]}]}`
}

func (f *bazarrFake) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *bazarrFake) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patches
}

func (f *bazarrFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	switch {
	case r.Method == http.MethodPatch:
		f.mu.Lock()
		f.patches++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/api/episodes":
		f.mu.Lock()
		f.reads++
		n := f.reads
		f.mu.Unlock()
		status, body := f.subs(n)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	case r.URL.Path == "/api/providers/episodes":
		_, _ = w.Write([]byte(f.candidates))
	default:
		http.NotFound(w, r)
	}
}

func newBazarrDisp(t *testing.T, f *bazarrFake, waits *agent.BazarrWaits) (*agent.Dispatcher, *atomic.Int64) {
	t.Helper()
	var arrHits atomic.Int64
	sonarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrHits.Add(1)
		switch r.URL.Path {
		case "/api/v3/series":
			_, _ = w.Write([]byte(`[{"id":1,"title":"Show"}]`))
		case "/api/v3/episode":
			_, _ = w.Write([]byte(`[{"id":42,"seriesId":1,"seasonNumber":1,"episodeNumber":2}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(sonarr.Close)
	bz := httptest.NewServer(f)
	t.Cleanup(bz.Close)
	return &agent.Dispatcher{
		Sonarr: client.NewArr(sonarr.URL, "k"), Bazarr: client.NewBazarr(bz.URL, "k"), BazarrWaits: waits,
	}, &arrHits
}

func searchArgs(language string) map[string]any {
	return map[string]any{
		"media_type": "tv", "title": "Show", "season": 1, "episode": 2, "language": language,
	}
}

func newSubsAfter(n int) func(int) (int, string) {
	return func(read int) (int, string) {
		if read >= n {
			return http.StatusOK, subsBody(oldSub, newSub)
		}
		return http.StatusOK, subsBody(oldSub)
	}
}

func runSearch(
	ctx context.Context, t *testing.T, disp *agent.Dispatcher, every, total time.Duration,
) (*agent.BazarrSearchResult, error) {
	t.Helper()
	out, err := disp.CallPolledForTest(ctx, every, total, "bazarr_search_subtitles", searchArgs("en"))
	if err != nil {
		return nil, err
	}
	res, ok := out.(*agent.BazarrSearchResult)
	if !ok {
		t.Fatalf("result type %T", out)
	}
	return res, nil
}

func TestC11_SearchFindsByPoll(t *testing.T) {
	t.Parallel()
	every, total := 5*time.Millisecond, 2*time.Second
	f := &bazarrFake{subs: newSubsAfter(3)}
	disp, _ := newBazarrDisp(t, f, &agent.BazarrWaits{})
	res, err := runSearch(t.Context(), t, disp, every, total)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Found || !strings.Contains(res.Summary, "Show.S01E02.en.srt") || f.patchCount() != 1 {
		t.Errorf("res %+v patches %d", res, f.patchCount())
	}
}

func TestC11_PushEndsPollEarly(t *testing.T) {
	t.Parallel()
	every, total := time.Hour, 5*time.Second
	f := &bazarrFake{}
	var ready atomic.Bool
	f.subs = func(int) (int, string) {
		if ready.Load() {
			return http.StatusOK, subsBody(oldSub, newSub)
		}
		return http.StatusOK, subsBody(oldSub)
	}
	waits := &agent.BazarrWaits{}
	disp, _ := newBazarrDisp(t, f, waits)
	start := time.Now()
	done := make(chan *agent.BazarrSearchResult, 1)
	go func() {
		res, _ := runSearch(t.Context(), t, disp, every, total)
		done <- res
	}()
	for f.readCount() < 2 {
		time.Sleep(time.Millisecond)
	}
	ready.Store(true)
	waits.Notify("ep:42")
	res := <-done
	if res == nil || !res.Found || time.Since(start) > time.Second {
		t.Errorf("res %+v after %v", res, time.Since(start))
	}
}

func TestC11_ReadyBeforeWait(t *testing.T) {
	t.Parallel()
	every, total := time.Hour, 5*time.Second
	f := &bazarrFake{subs: newSubsAfter(2)}
	disp, _ := newBazarrDisp(t, f, &agent.BazarrWaits{})
	start := time.Now()
	res, err := runSearch(t.Context(), t, disp, every, total)
	if err != nil || !res.Found || time.Since(start) > time.Second {
		t.Errorf("res %+v err %v after %v", res, err, time.Since(start))
	}
}

func TestC11_OverwriteCounts(t *testing.T) {
	t.Parallel()
	every, total := 5*time.Millisecond, 2*time.Second
	f := &bazarrFake{subs: func(read int) (int, string) {
		if read >= 2 {
			return http.StatusOK, subsBody(`{"code2":"en","path":"/tv/Show.en.srt","file_size":999}`)
		}
		return http.StatusOK, subsBody(oldSub)
	}}
	disp, _ := newBazarrDisp(t, f, &agent.BazarrWaits{})
	res, err := runSearch(t.Context(), t, disp, every, total)
	if err != nil || !res.Found {
		t.Errorf("res %+v err %v", res, err)
	}
}

func TestC11_NothingFoundListsCandidates(t *testing.T) {
	t.Parallel()
	every, total := 5*time.Millisecond, 50*time.Millisecond
	var cands []string
	for i := range 7 {
		cands = append(cands, fmt.Sprintf(
			`{"provider":"p","release_info":["r%d","x"],"score":%d,"language":"en"}`, i, 50+i))
	}
	cands = append(cands, `{"provider":"p","release_info":["top"],"score":92.5,"language":"en"}`,
		`{"provider":"p","release_info":["fr"],"score":99,"language":"fr"}`)
	f := &bazarrFake{subs: newSubsAfter(1000), candidates: "[" + strings.Join(cands, ",") + "]"}
	disp, _ := newBazarrDisp(t, f, &agent.BazarrWaits{})
	res, err := runSearch(t.Context(), t, disp, every, total)
	if err != nil {
		t.Fatal(err)
	}
	if res.Found || len(res.Candidates) != 5 || res.Candidates[0].Score != 92.5 || res.Candidates[0].Release != "top" {
		t.Fatalf("res %+v", res)
	}
	for i, c := range res.Candidates {
		if c.Language != "en" || (i > 0 && c.Score > res.Candidates[i-1].Score) {
			t.Errorf("candidate %d = %+v", i, c)
		}
	}
	if !strings.Contains(res.Summary, "5 candidates on the card") {
		t.Errorf("summary %q", res.Summary)
	}
}

func TestC11_PushWithoutNewFileKeepsPolling(t *testing.T) {
	t.Parallel()
	every, total := time.Hour, 200*time.Millisecond
	f := &bazarrFake{subs: newSubsAfter(1000), candidates: "[]"}
	waits := &agent.BazarrWaits{}
	disp, _ := newBazarrDisp(t, f, waits)
	go func() {
		for f.readCount() < 2 {
			time.Sleep(time.Millisecond)
		}
		waits.Notify("ep:42")
	}()
	res, err := runSearch(t.Context(), t, disp, every, total)
	if err != nil || res.Found {
		t.Fatalf("res %+v err %v", res, err)
	}
	if n := f.readCount(); n > 3 {
		t.Errorf("subtitle reads = %d, want at most 3", n)
	}
}

func TestC11_StopEndsPoll(t *testing.T) {
	t.Parallel()
	every, total := time.Hour, 5*time.Second
	f := &bazarrFake{subs: newSubsAfter(1000)}
	disp, _ := newBazarrDisp(t, f, &agent.BazarrWaits{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := runSearch(ctx, t, disp, every, total)
		errc <- err
	}()
	for f.readCount() < 2 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	cancel()
	err := <-errc
	if !errors.Is(err, context.Canceled) || time.Since(start) > 100*time.Millisecond || f.patchCount() != 1 {
		t.Errorf("err %v after %v, patches %d", err, time.Since(start), f.patchCount())
	}
}

func TestC11_ReadErrorAfterPatchKeepsPolling(t *testing.T) {
	t.Parallel()
	every, total := 5*time.Millisecond, 2*time.Second
	f := &bazarrFake{subs: func(read int) (int, string) {
		switch {
		case read == 2:
			return http.StatusInternalServerError, "boom"
		case read >= 3:
			return http.StatusOK, subsBody(oldSub, newSub)
		}
		return http.StatusOK, subsBody(oldSub)
	}}
	disp, _ := newBazarrDisp(t, f, &agent.BazarrWaits{})
	res, err := runSearch(t.Context(), t, disp, every, total)
	if err != nil || !res.Found {
		t.Errorf("res %+v err %v", res, err)
	}
}

func TestC11_NilWaitsPollsOnly(t *testing.T) {
	t.Parallel()
	every, total := 5*time.Millisecond, 2*time.Second
	f := &bazarrFake{subs: newSubsAfter(3)}
	disp, _ := newBazarrDisp(t, f, nil)
	res, err := runSearch(t.Context(), t, disp, every, total)
	if err != nil || !res.Found {
		t.Errorf("res %+v err %v", res, err)
	}
}

func TestC11_UnknownLanguageNoCalls(t *testing.T) {
	t.Parallel()
	f := &bazarrFake{subs: newSubsAfter(1)}
	disp, arrHits := newBazarrDisp(t, f, &agent.BazarrWaits{})
	_, err := disp.Call(t.Context(), "bazarr_search_subtitles", searchArgs("zz"))
	if !errors.Is(err, agent.ErrUnknownLanguage) {
		t.Errorf("err = %v", err)
	}
	if n := f.requests.Load() + arrHits.Load(); n != 0 {
		t.Errorf("%d requests", n)
	}
}

func TestC11_IsAutonomousNotDisruptive(t *testing.T) {
	t.Parallel()
	spec, ok := agent.SpecByNameForTest("bazarr_search_subtitles")
	if !ok || !agent.IsAutonomousActionForTest("bazarr_search_subtitles") || agent.DisruptiveForTest(spec) {
		t.Errorf("spec ok=%v", ok)
	}
}

func TestC11_LanguageIsIdentity(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "subtitles", Title: "Show"}
	lifecycletest.Incident(t, database, inc)
	if err := database.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "bazarr_search_subtitles", TriggeredBy: "agent", Status: db.ActionApplied,
		Params: map[string]any{
			"media_type": "tv", "title": "Show", "season": float64(1), "episode": float64(2), "language": "en",
		},
	}); err != nil {
		t.Fatal(err)
	}
	a := agent.New(nil, "", nil, database, nil, nil)
	args := func(lang string) string {
		return `{"media_type":"tv","title":"Show","season":1,"episode":2,"language":"` + lang + `"}`
	}
	if !a.ActionAlreadyAppliedForTest(ctx, inc.ID, "bazarr_search_subtitles", args("english")) {
		t.Error("en and english should be one target")
	}
	if a.ActionAlreadyAppliedForTest(ctx, inc.ID, "bazarr_search_subtitles", args("ja")) {
		t.Error("en and ja should be two targets")
	}
}

func bazarrReply(id string, res any) (openai.ChatCompletionMessage, openai.ChatCompletionMessage) {
	fn := openai.FunctionCall{Name: "bazarr_search_subtitles", Arguments: `{}`}
	call := openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		ToolCalls: []openai.ToolCall{{ID: id, Type: openai.ToolTypeFunction, Function: fn}},
	}
	body, _ := json.Marshal(res)
	return call, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: string(body)}
}

func completeCall(extra map[string]any) openai.ToolCall {
	args := map[string]any{
		"root_cause": "x", "confidence": "high", "primary_action": "bazarr", "primary_reason": "x",
		"verify_after_seconds": 60, "verify_subtitle_language": "en",
	}
	maps.Copy(args, extra)
	b, _ := json.Marshal(args)
	return openai.ToolCall{ID: "cd", Function: openai.FunctionCall{Name: "complete_diagnosis", Arguments: string(b)}}
}

func TestC11_ConcludeCarriesSearch(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{"{}"}})
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "subtitles", Title: "Show"}
	lifecycletest.Incident(t, database, inc)
	stored := agent.BazarrSearchResult{
		Summary: "No en subtitles found automatically; 2 candidates on the card", Waited: "90s", Title: "Show",
		Season: 1, Episode: 2,
		Candidates: []client.BazarrCandidate{
			{Provider: "p", Release: "rel-a", Score: 90, Language: "en"},
			{Provider: "p", Release: "rel-b", Score: 80, Language: "en"},
		},
	}
	var msgs []openai.ChatCompletionMessage
	c1, r1 := bazarrReply("b1", stored)
	c2, r2 := bazarrReply("b2", map[string]any{"error": "not run: autonomous actions paused"})
	msgs = append(msgs, c1, r1, c2, r2)
	forged := map[string]any{"found": true, "summary": "forged"}
	res, err := ag.ConcludeForTest(
		t.Context(),
		inc,
		completeCall(map[string]any{"subtitle_search": forged}),
		&stubGate{},
		&msgs,
	)
	if err != nil || res == nil {
		t.Fatalf("conclude: %v %v", res, err)
	}
	got, _ := json.Marshal(res.SubtitleSearch)
	want, _ := json.Marshal(&stored)
	if string(got) != string(want) {
		t.Errorf("SubtitleSearch = %s, want %s", got, want)
	}
	events, err := journal.New(database).Events(t.Context(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == string(journal.KindDiagnosisCompleted) {
			found = strings.Contains(string(e.Payload), `"subtitle_search"`) &&
				strings.Contains(string(e.Payload), "rel-a")
		}
	}
	if !found {
		t.Error("journalled diagnosis lacks the search")
	}
}

func TestC11_SubtitleItemFromSearch(t *testing.T) {
	t.Parallel()
	items := map[string]client.ItemInfo{
		"ep1":   {ID: "ep1", Type: "Episode", SeriesID: "s1", SeriesName: "Show", Season: 1, Episode: 2},
		"other": {ID: "other", Type: "Episode", SeriesID: "s2", SeriesName: "Else", Season: 1, Episode: 2},
		"mv":    {ID: "mv", Type: "Movie", Name: "Film"},
		"s1":    {ID: "s1", Type: "Series"},
	}
	ag := newItemJellyfin(t, items)
	tv := &agent.BazarrSearchResult{Summary: "x", Title: "show", Season: 1, Episode: 2}
	movie := &agent.BazarrSearchResult{Summary: "x", Title: "FILM", Movie: true}
	otherMovie := &agent.BazarrSearchResult{Summary: "x", Title: "Other Film", Movie: true}
	for _, c := range []struct {
		name    string
		incItem string
		verify  string
		search  *agent.BazarrSearchResult
		aligned []string
		want    string
	}{
		{"empty incident, searched episode", "", "ep1", tv, nil, "ep1"},
		{"series incident, searched episode", "s1", "ep1", tv, nil, "ep1"},
		{"same season and episode of another series", "", "other", tv, nil, ""},
		{"movie after a movie search", "", "mv", movie, nil, "mv"},
		{"movie after a tv search", "", "mv", tv, nil, ""},
		{"movie after another movie's search", "", "mv", otherMovie, nil, ""},
		{"movie as an aligned item", "", "mv", nil, []string{"mv"}, "mv"},
		{"rerun with the repeat search blocked", "", "ep1", nil, []string{"ep1"}, "ep1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inc := &db.Incident{ID: "inc", What: agent.WhatSubtitles, JellyfinItemID: c.incItem, Title: "Show"}
			var msgs []openai.ChatCompletionMessage
			for _, id := range c.aligned {
				call, reply := alignCall(id)
				msgs = append(msgs, call, reply)
			}
			if c.search != nil {
				call, reply := bazarrReply("b1", c.search)
				msgs = append(msgs, call, reply)
			} else {
				call, reply := bazarrReply("b0", map[string]any{"error": "not run: repeat"})
				msgs = append(msgs, call, reply)
			}
			res, err := ag.ConcludeForTest(
				t.Context(),
				inc,
				completeCall(map[string]any{"verify_item_id": c.verify}),
				&stubGate{},
				&msgs,
			)
			if err != nil || res == nil {
				t.Fatalf("conclude: %v %v", res, err)
			}
			if res.SubtitleItemID != c.want {
				t.Errorf("SubtitleItemID = %q, want %q", res.SubtitleItemID, c.want)
			}
		})
	}
}
