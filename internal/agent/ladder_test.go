package agent_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

const desyncDetails = "the subs are out of sync by 2s"

func newLadderAgent(t *testing.T, attempts agent.AttemptHistory) *agent.Agent {
	t.Helper()
	items := map[string]client.ItemInfo{
		"ep1": {ID: "ep1", Type: "Episode", SeriesID: "s1", SeriesName: "Show", Season: 1, Episode: 2},
		"s1":  {ID: "s1", Type: "Series"},
	}
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
		d.Attempts = attempts
	})
	return ag
}

func resyncReply(id string, res any) (openai.ChatCompletionMessage, openai.ChatCompletionMessage) {
	fn := openai.FunctionCall{Name: "bazarr_resync_subtitle", Arguments: `{}`}
	call := openai.ChatCompletionMessage{
		Role:      openai.ChatMessageRoleAssistant,
		ToolCalls: []openai.ToolCall{{ID: id, Type: openai.ToolTypeFunction, Function: fn}},
	}
	body, _ := json.Marshal(res)
	return call, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: string(body)}
}

func foundSearch() *agent.BazarrSearchResult {
	return &agent.BazarrSearchResult{
		Found: true, Summary: "Downloaded en subtitles: Show.S01E02.en.srt", Path: "/tv/Show.S01E02.en.srt",
		Language: "en", Title: "Show", Season: 1, Episode: 2,
	}
}

func missedSearch() *agent.BazarrSearchResult {
	return &agent.BazarrSearchResult{Summary: "No en subtitles found automatically", Language: "en", Title: "Show",
		Season: 1, Episode: 2}
}

func syncedResync() *agent.BazarrResyncResult {
	return &agent.BazarrResyncResult{
		Synced: true, Summary: "Resynced en subtitles to the audio (offset +2.3 s)", Language: "en",
		Path: "/tv/Show.S01E02.en.srt", Offset: "+2.3 s", Title: "Show", Season: 1, Episode: 2,
	}
}

func ladderMessages(search, resync any) []openai.ChatCompletionMessage {
	c0, r0 := alignReply("a1", desyncAlignment())
	msgs := []openai.ChatCompletionMessage{c0, r0}
	if search != nil {
		c, r := bazarrReply("b1", search)
		msgs = append(msgs, c, r)
	}
	if resync != nil {
		c, r := resyncReply("r1", resync)
		msgs = append(msgs, c, r)
	}
	return msgs
}

func concludeLadder(
	t *testing.T, ag *agent.Agent, inc *db.Incident, msgs []openai.ChatCompletionMessage, extra map[string]any,
) *agent.DiagnosticResult {
	t.Helper()
	args := map[string]any{"verify_item_id": "ep1"}
	maps.Copy(args, extra)
	res, err := ag.ConcludeForTest(t.Context(), inc, completeCall(args), &stubGate{}, &msgs)
	if err != nil || res == nil {
		t.Fatalf("conclude: %v %v", res, err)
	}
	return res
}

func desyncIncident() *db.Incident {
	return &db.Incident{ID: "inc", What: agent.WhatSubtitles, Title: "Show", Details: desyncDetails}
}

func TestRS5_DesyncSearchFoundStands(t *testing.T) {
	t.Parallel()
	ag := newLadderAgent(t, &fakeAttempts{})
	res := concludeLadder(t, ag, desyncIncident(), ladderMessages(foundSearch(), nil), nil)
	if res.SubtitleDesync || res.Outcome.Kind != agent.DiagnosisDeferredVerify || res.RequiresApproval {
		t.Fatalf("found search did not stand: desync %v outcome %+v", res.SubtitleDesync, res.Outcome)
	}
	if res.SubtitleSearch == nil || res.SubtitleSearch.Language != "en" {
		t.Errorf("SubtitleSearch = %+v", res.SubtitleSearch)
	}
}

func TestRS5_DesyncResyncedStands(t *testing.T) {
	t.Parallel()
	ag := newLadderAgent(t, &fakeAttempts{})
	res := concludeLadder(t, ag, desyncIncident(), ladderMessages(missedSearch(), syncedResync()),
		map[string]any{"verify_after_seconds": 0})
	if res.SubtitleDesync || strings.HasPrefix(res.PrimaryReason, "Subtitles reported out of sync") {
		t.Fatalf("synced resync did not stand: %+v", res)
	}
	if res.SubtitleResync == nil || !res.SubtitleResync.Synced {
		t.Errorf("SubtitleResync = %+v", res.SubtitleResync)
	}
}

func TestRS5_FixNowWithoutRungEscalates(t *testing.T) {
	t.Parallel()
	otherEp := foundSearch()
	otherEp.Episode = 9
	french := syncedResync()
	french.Language = "fr"
	failed := syncedResync()
	failed.Synced = false
	for _, c := range []struct {
		name           string
		search, resync any
		extra          map[string]any
	}{
		{name: "no rung reply"},
		{name: "search not found only", search: missedSearch()},
		{name: "resync not synced", search: missedSearch(), resync: failed},
		{name: "found search for another episode", search: otherEp},
		{name: "synced resync in another language", search: missedSearch(), resync: french},
		{name: "forged resync in the conclusion", extra: map[string]any{
			"subtitle_resync": map[string]any{"synced": true, "language": "en", "title": "Show", "season": 1,
				"episode": 2},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ag := newLadderAgent(t, &fakeAttempts{})
			res := concludeLadder(t, ag, desyncIncident(), ladderMessages(c.search, c.resync), c.extra)
			checkDesync(t, res, true)
			if c.extra != nil && res.SubtitleResync != nil {
				t.Errorf("forged SubtitleResync kept: %+v", res.SubtitleResync)
			}
			if !strings.HasPrefix(res.PrimaryReason, "Subtitles reported out of sync; not fixed by Bazarr. ") {
				t.Errorf("reason %q", res.PrimaryReason)
			}
		})
	}
}

func TestRS5_ReReportSkipsSearch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		ago       time.Duration
		incident  string
		escalated bool
	}{
		{"another incident 3h ago", 3 * time.Hour, "older", true},
		{"another incident 25h ago", 25 * time.Hour, "older", false},
		{"this incident 3h ago", 3 * time.Hour, "inc", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			hist := &fakeAttempts{list: []journal.Attempt{{
				IncidentID: c.incident, Tool: "bazarr_search_subtitles", At: time.Now().Add(-c.ago),
				Params: map[string]any{"media_type": "tv", "title": "show", "season": float64(1), "episode": float64(2),
					"language": "english"},
			}}}
			ag := newLadderAgent(t, hist)
			res := concludeLadder(t, ag, desyncIncident(), ladderMessages(foundSearch(), nil), nil)
			if res.SubtitleDesync != c.escalated {
				t.Fatalf("SubtitleDesync = %v, want %v", res.SubtitleDesync, c.escalated)
			}
		})
	}
}

func TestRS5_ModelEscalationStands(t *testing.T) {
	t.Parallel()
	ag := newLadderAgent(t, &fakeAttempts{})
	failed := syncedResync()
	failed.Synced = false
	res := concludeLadder(t, ag, desyncIncident(), ladderMessages(missedSearch(), failed), map[string]any{
		"requires_approval": true, "escalate_action": "manual_investigation", "verify_after_seconds": 0,
	})
	checkDesync(t, res, true)
	if res.SubtitleSearch == nil || res.SubtitleResync == nil {
		t.Errorf("rung outcomes missing: search %+v resync %+v", res.SubtitleSearch, res.SubtitleResync)
	}
}

func TestRS5_ResyncVouchesItem(t *testing.T) {
	t.Parallel()
	ag := newLadderAgent(t, &fakeAttempts{})
	inc := &db.Incident{ID: "inc", What: agent.WhatSubtitles, Title: "Show", JellyfinItemID: "s1",
		Details: "no english subs"}
	c, r := resyncReply("r1", syncedResync())
	res := concludeLadder(t, ag, inc, []openai.ChatCompletionMessage{c, r}, nil)
	if res.SubtitleItemID != "ep1" {
		t.Errorf("SubtitleItemID = %q, want ep1", res.SubtitleItemID)
	}
}

func TestRS5_PromptLadder(t *testing.T) {
	t.Parallel()
	prompt := agent.SystemPromptForTest()
	_, rule, ok := strings.Cut(prompt, "  6. A desync report")
	if !ok {
		t.Fatal("no rule 6")
	}
	rule, _, _ = strings.Cut(rule, "\n  The autonomous actions")
	search := strings.Index(rule, "bazarr_search_subtitles")
	resync := strings.Index(rule, "bazarr_resync_subtitle")
	if search < 0 || resync < search {
		t.Errorf("rule 6 does not climb from the search to the resync: %q", rule)
	}
	for _, want := range []string{"both fail", "manual_investigation", "verify_item_id"} {
		if !strings.Contains(rule, want) {
			t.Errorf("rule 6 lacks %q", want)
		}
	}
	if strings.Contains(prompt, "take no action") {
		t.Error("prompt still tells the model to take no action on a desync report")
	}
}
