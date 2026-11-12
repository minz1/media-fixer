package agent_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
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

type vstream struct {
	index    int
	external bool
	lang     string
	status   int
	body     string
	slow     time.Duration
}

func newVerifyJellyfin(t *testing.T, size int64, streams []vstream) *agent.Agent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Users":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "admin-1"}})
		case strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
			var ms []client.MediaStream
			for _, s := range streams {
				ms = append(ms, client.MediaStream{
					Index: s.index, Type: "Subtitle", Language: s.lang, IsExternal: s.external,
					IsTextSubtitleStream: true,
				})
			}
			_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{MediaSources: []client.MediaSource{{
				ID: "src1", Size: size, MediaStreams: ms,
			}}})
		default:
			for _, s := range streams {
				if strings.Contains(r.URL.Path, "/Subtitles/"+strconv.Itoa(s.index)+"/") {
					select {
					case <-time.After(s.slow):
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(s.status)
					_, _ = w.Write([]byte(s.body))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	disp := &agent.Dispatcher{Jellyfin: client.NewJellyfin(srv.URL, "k")}
	return agent.New(nil, "", disp, nil, nil, slog.New(slog.DiscardHandler))
}

func TestB6_VerifySubtitlePasses(t *testing.T) {
	t.Parallel()
	ag := newVerifyJellyfin(t, 1<<30, []vstream{{index: 5, external: true, lang: "eng", status: 200, body: srtBody(4)}})
	if !ag.VerifySubtitle(t.Context(), "ep1", "en") {
		t.Error("one external eng stream with 4 english cues did not pass")
	}
	ag = newVerifyJellyfin(t, 1<<30, []vstream{
		{index: 3, lang: "eng", status: 500},
		{index: 5, external: true, lang: "eng", status: 200, body: srtBody(4)},
	})
	if !ag.VerifySubtitle(t.Context(), "ep1", "eng") {
		t.Error("external stream was not tried first")
	}
}

func TestB6_VerifySubtitleFails(t *testing.T) {
	t.Parallel()
	ja := strings.Repeat("00:00:01,000 --> 00:00:02,000\nこれはテストです 私は学生です\n\n", 6)
	big := int64(21 << 30)
	for _, tc := range []struct {
		name    string
		size    int64
		streams []vstream
		timeout time.Duration
		want    bool
	}{
		{"japanese text tagged eng", 1 << 30, []vstream{{index: 3, lang: "eng", status: 200, body: ja}}, 0, false},
		{"two cues", 1 << 30, []vstream{{index: 3, lang: "eng", status: 200, body: srtBody(2)}}, 0, false},
		{
			"timeout", 1 << 30,
			[]vstream{{index: 3, lang: "eng", status: 200, body: srtBody(4), slow: 5 * time.Second}},
			50 * time.Millisecond, false,
		},
		{"no eng stream", 1 << 30, []vstream{{index: 3, lang: "jpn", status: 200, body: srtBody(4)}}, 0, false},
		{"embedded on a 21 GB source", big, []vstream{{index: 3, lang: "eng", status: 200, body: srtBody(4)}}, 0, false},
		{
			"external sidecar on a 21 GB source", big,
			[]vstream{{index: 6, external: true, lang: "eng", status: 200, body: srtBody(4)}}, 0, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			if got := newVerifyJellyfin(t, tc.size, tc.streams).VerifySubtitle(ctx, "ep1", "eng"); got != tc.want {
				t.Errorf("VerifySubtitle = %v, want %v", got, tc.want)
			}
		})
	}
}

func newItemJellyfin(t *testing.T, items map[string]client.ItemInfo) *agent.Agent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users" {
			_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "admin-1"}})
			return
		}
		it, ok := items[r.URL.Query().Get("ids")]
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": []client.ItemInfo{it}})
	}))
	t.Cleanup(srv.Close)
	ag, _ := newRunTestAgent(t, &scriptedLLM{responses: []string{"{}"}}, func(d *agent.Dispatcher) {
		d.Jellyfin = client.NewJellyfin(srv.URL, "k")
	})
	return ag
}

func alignCall(itemID string) (openai.ChatCompletionMessage, openai.ChatCompletionMessage) {
	id := "a-" + itemID
	fn := openai.FunctionCall{Name: "stream_alignment", Arguments: `{"item_id":"` + itemID + `","language":"en"}`}
	tc := openai.ToolCall{ID: id, Type: openai.ToolTypeFunction, Function: fn}
	call := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{tc}}
	reply := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: `{"streams":[]}`}
	return call, reply
}

func TestB6_SubtitleItemResolution(t *testing.T) {
	t.Parallel()
	items := map[string]client.ItemInfo{
		"ep1":   {ID: "ep1", Type: "Episode", SeriesID: "s1", SeriesName: "Show", Season: 1, Episode: 1},
		"ep2":   {ID: "ep2", Type: "Episode", SeriesID: "s1", SeriesName: "Show", Season: 1, Episode: 2},
		"other": {ID: "other", Type: "Episode", SeriesID: "s2", SeriesName: "Else", Season: 1, Episode: 1},
		"s1":    {ID: "s1", Type: "Series"},
	}
	ag := newItemJellyfin(t, items)
	type tc struct {
		name    string
		incItem string
		verify  string
		aligned []string
		errored []string
		want    string
	}
	for _, c := range []tc{
		{"episode incident wins over the model", "ep1", "ep2", nil, nil, "ep1"},
		{"series incident, aligned episode", "s1", "ep1", []string{"ep1"}, nil, "ep1"},
		{"series incident, sibling not aligned", "s1", "ep2", []string{"ep1"}, nil, ""},
		{"series incident, other series episode", "s1", "other", []string{"other"}, nil, ""},
		{"series incident, errored alignment", "s1", "ep1", nil, []string{"ep1"}, ""},
		{"empty incident, aligned episode", "", "ep1", []string{"ep1"}, nil, "ep1"},
		{"empty incident, other episode", "", "ep2", []string{"ep1"}, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inc := &db.Incident{ID: "inc", What: agent.WhatSubtitles, JellyfinItemID: c.incItem, Title: "Show"}
			var msgs []openai.ChatCompletionMessage
			for _, id := range c.aligned {
				call, reply := alignCall(id)
				msgs = append(msgs, call, reply)
			}
			for _, id := range c.errored {
				call, reply := alignCall(id)
				reply.Content = `{"error":"boom"}`
				msgs = append(msgs, call, reply)
			}
			args, _ := json.Marshal(map[string]any{
				"root_cause": "x", "confidence": "high", "primary_action": "bazarr", "primary_reason": "x",
				"verify_after_seconds": 60, "verify_item_id": c.verify, "subtitle_item_id": "forged",
				"verify_subtitle_language": "en",
			})
			fn := openai.FunctionCall{Name: "complete_diagnosis", Arguments: string(args)}
			call := openai.ToolCall{ID: "cd", Function: fn}
			res, err := ag.ConcludeForTest(t.Context(), inc, call, &stubGate{}, &msgs)
			if err != nil || res == nil {
				t.Fatalf("conclude: %v %v", res, err)
			}
			if res.SubtitleItemID != c.want {
				t.Errorf("SubtitleItemID = %q, want %q", res.SubtitleItemID, c.want)
			}
		})
	}
}

func TestB6_NoUsableLanguageResolvesNoItem(t *testing.T) {
	t.Parallel()
	ag := newItemJellyfin(t, map[string]client.ItemInfo{"ep1": {ID: "ep1", Type: "Episode"}})
	for _, lang := range []string{"", "korean"} {
		inc := &db.Incident{ID: "inc", What: agent.WhatSubtitles, JellyfinItemID: "ep1", Title: "Show"}
		args, _ := json.Marshal(map[string]any{
			"root_cause": "x", "confidence": "high", "primary_action": "bazarr", "primary_reason": "x",
			"verify_after_seconds": 60, "verify_subtitle_language": lang,
		})
		call := openai.ToolCall{
			ID:       "cd",
			Function: openai.FunctionCall{Name: "complete_diagnosis", Arguments: string(args)},
		}
		var msgs []openai.ChatCompletionMessage
		res, err := ag.ConcludeForTest(t.Context(), inc, call, &stubGate{}, &msgs)
		if err != nil || res == nil {
			t.Fatalf("conclude %q: %v %v", lang, res, err)
		}
		if res.SubtitleItemID != "" {
			t.Errorf("language %q: SubtitleItemID = %q, want none", lang, res.SubtitleItemID)
		}
	}
}

type stubGate struct{}

func (*stubGate) Claim(context.Context) error { return nil }

func (*stubGate) Act(context.Context, string, map[string]any) (agent.Verdict, error) {
	return agent.GateAllowed, nil
}
func (*stubGate) Gen() int64                         { return 1 }
func (*stubGate) ActionLogged(context.Context)       {}
func (*stubGate) Live(context.Context) agent.Verdict { return agent.GateAllowed }
func (*stubGate) Calls() []agent.CallRecord          { return nil }
func (*stubGate) Record(agent.CallRecord)            {}

func TestB6_FixNowSubtitleVerify(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		body   string
		wantOK bool
	}{
		{"passes", srtBody(4), true},
		{"fails", srtBody(1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotPath atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/Users":
					_ = json.NewEncoder(w).Encode([]map[string]string{{"Id": "admin-1"}})
				case r.URL.Path == "/Items":
					it := []client.ItemInfo{{ID: "ep1", Type: "Episode"}}
					_ = json.NewEncoder(w).Encode(map[string]any{"Items": it})
				case strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
					_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{MediaSources: []client.MediaSource{{
						ID: "src1", Size: 1 << 30, MediaStreams: []client.MediaStream{
							{Index: 3, Type: "Subtitle", Language: "eng", IsExternal: true},
						},
					}}})
				default:
					gotPath.Store(r.URL.Path)
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			t.Cleanup(srv.Close)
			ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{toolCallResponse("complete_diagnosis",
				`{"root_cause":"x","confidence":"high","primary_action":"bazarr","primary_reason":"x",`+
					`"requires_approval":false,"verify_subtitle_language":"en"}`)}},
				func(d *agent.Dispatcher) { d.Jellyfin = client.NewJellyfin(srv.URL, "k") })
			inc := seedRunIncident(t, database)
			inc.What = agent.WhatSubtitles
			inc.JellyfinItemID = "ep1"
			result, _, err := runOnce(t.Context(), t, ag, inc)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if p, _ := gotPath.Load().(string); !strings.Contains(p, "/Subtitles/3/") {
				t.Errorf("subtitle was not extracted, last path %q", p)
			}
			escalated := result.RequiresApproval && result.EscalateAction == agent.EscalateManualInvestigation
			if escalated == tc.wantOK {
				t.Errorf("RequiresApproval=%v EscalateAction=%q", result.RequiresApproval, result.EscalateAction)
			}
		})
	}
}

func TestB8_PromptSubtitleProtocolB(t *testing.T) {
	t.Parallel()
	prompt := agent.SystemPromptForTest()
	_, rest, ok := strings.Cut(prompt, "--- Subtitles (what=subtitles) ---")
	if !ok {
		t.Fatal("no subtitles section")
	}
	section, _, _ := strings.Cut(rest, "\n--- ")
	for _, want := range []string{
		"stream_alignment", "index_drift", "list_alternatives", "subtitle_extract_test",
		"verify_subtitle_language", "verify_item_id",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("subtitles section lacks %q", want)
		}
	}
}

func TestC13_PromptBazarrStep(t *testing.T) {
	t.Parallel()
	prompt := agent.SystemPromptForTest()
	_, rest, ok := strings.Cut(prompt, "--- Subtitles (what=subtitles) ---")
	if !ok {
		t.Fatal("no subtitles section")
	}
	section, _, _ := strings.Cut(rest, "\n--- ")
	for _, want := range []string{
		"bazarr_search_subtitles", "primary_reason", "verify_after_seconds", "60",
		"index_drift", "subtitle_extract_test", "already applied", "stream_alignment",
		"verify_subtitle_language", "verify_item_id", "manual_investigation",
		"Both autonomous actions",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("subtitles section lacks %q", want)
		}
	}
	if strings.Contains(section, "The only autonomous action") {
		t.Error("section still says clear_jellyfin_cache is the only autonomous action")
	}
	if n := strings.Count(section, "bazarr_search_subtitles forces"); n != 0 {
		t.Errorf("interim line left in place: %d", n)
	}
}
