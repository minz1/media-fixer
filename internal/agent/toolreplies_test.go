package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

const diagnosisArgs = `{"root_cause":"x","confidence":"high","primary_action":"decypharr_recheck",` +
	`"primary_reason":"x","requires_approval":true,"escalate_action":"none"}`

type call struct {
	name, args string
}

func batchResponse(calls ...call) string {
	tc := make([]map[string]any, 0, len(calls))
	for i, c := range calls {
		tc = append(tc, map[string]any{
			"id":       "call_" + strings.Repeat("x", i) + c.name,
			"type":     "function",
			"function": map[string]string{"name": c.name, "arguments": c.args},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"id": "c", "object": "chat.completion", "model": "stub",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{"role": "assistant", "tool_calls": tc},
		}},
	})
	return string(b)
}

type mediaAgentStub struct {
	mu    sync.Mutex
	paths []string
	onHit func()
}

func (m *mediaAgentStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.paths = append(m.paths, r.URL.Path)
	hit := m.onHit
	m.mu.Unlock()
	if hit != nil {
		hit()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (m *mediaAgentStub) hits() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.paths...)
}

func withMediaAgent(t *testing.T, stub *mediaAgentStub) func(*agent.Dispatcher) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return func(d *agent.Dispatcher) { d.MediaAgent = client.NewMediaAgent(srv.URL, "key") }
}

func unpaired(msgs []openai.ChatCompletionMessage) []string {
	var missing []string
	for i, m := range msgs {
		for _, c := range m.ToolCalls {
			answered := false
			for _, r := range msgs[i+1:] {
				if r.Role == openai.ChatMessageRoleTool && r.ToolCallID == c.ID {
					answered = true
					break
				}
			}
			if !answered {
				missing = append(missing, c.ID)
			}
		}
	}
	return missing
}

func toolReply(msgs []openai.ChatCompletionMessage, id string) string {
	for _, m := range msgs {
		if m.Role == openai.ChatMessageRoleTool && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

func storedConversation(t *testing.T, database *db.DB, id string) []openai.ChatCompletionMessage {
	t.Helper()
	msgs, err := journal.New(database).Conversation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestA3_7_CompleteDiagnosisAnswersEveryToolCall(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{responses: []string{
		batchResponse(call{"complete_diagnosis", diagnosisArgs}, call{"get_disk_info", `{}`}),
	}}
	stub := &mediaAgentStub{}
	ag, database := newRunTestAgent(t, llm, withMediaAgent(t, stub))
	inc := seedRunIncident(t, database)

	result, conv, err := runOnce(context.Background(), ag, inc)
	if err != nil || result == nil {
		t.Fatalf("Run = %+v, %v", result, err)
	}
	for name, msgs := range map[string][]openai.ChatCompletionMessage{
		"returned": conv, "journal": storedConversation(t, database, inc.ID),
	} {
		if missing := unpaired(msgs); len(missing) != 0 {
			t.Errorf("%s conversation leaves %v unanswered", name, missing)
		}
		if got := toolReply(msgs, "call_complete_diagnosis"); got != `{"status":"recorded"}` {
			t.Errorf("%s complete_diagnosis reply = %q", name, got)
		}
		if got := toolReply(
			msgs,
			"call_xget_disk_info",
		); !strings.Contains(
			got,
			"not run: complete_diagnosis ended the run",
		) {
			t.Errorf("%s get_disk_info reply = %q, want not run", name, got)
		}
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("media agent got %v, want get_disk_info never dispatched", hits)
	}
}

func TestA6_10_BudgetTripAnswersDroppedCalls(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{responses: []string{
		batchResponse(call{"clear_jellyfin_cache", `{"item_id":"abc"}`}, call{"restart_decypharr", `{}`}),
	}}
	stub := &mediaAgentStub{}
	ag, database := newRunTestAgent(t, llm, withMediaAgent(t, stub))
	inc := seedRunIncident(t, database)
	ctx := context.Background()
	for range 3 {
		if _, err := database.IncrementActionCount(ctx, inc.ID); err != nil {
			t.Fatal(err)
		}
	}

	_, conv, err := runOnce(ctx, ag, inc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for name, msgs := range map[string][]openai.ChatCompletionMessage{
		"returned": conv, "journal": storedConversation(t, database, inc.ID),
	} {
		if missing := unpaired(msgs); len(missing) != 0 {
			t.Errorf("%s conversation leaves %v unanswered", name, missing)
		}
		for _, id := range []string{"call_clear_jellyfin_cache", "call_xrestart_decypharr"} {
			if got := toolReply(msgs, id); !strings.Contains(got, "not run: autonomous action limit reached") {
				t.Errorf("%s %s reply = %q, want not run", name, id, got)
			}
		}
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("media agent got %v, want no restart sent", hits)
	}
}

func TestAnswerPending_LeavesAnsweredCallsAlone(t *testing.T) {
	t.Parallel()
	calls := []openai.ToolCall{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleTool, ToolCallID: "c", Content: "old round"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: calls},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "a", Content: "done"},
	}
	got := agent.AnswerPendingForTest(msgs, calls, "the run was stopped")
	if len(got) != 5 {
		t.Fatalf("got %d messages, want 5: %+v", len(got), got)
	}
	if got[2].Content != "done" {
		t.Errorf("answered call rewritten: %q", got[2].Content)
	}
	for i, id := range []string{"b", "c"} {
		m := got[3+i]
		if m.Role != openai.ChatMessageRoleTool || m.ToolCallID != id ||
			m.Content != `{"error":"not run: the run was stopped"}` {
			t.Errorf("reply %d = %+v, want not run for %s", i, m, id)
		}
	}
	if again := agent.AnswerPendingForTest(got, calls, "x"); len(again) != len(got) {
		t.Errorf("a second pass added %d replies", len(again)-len(got))
	}
}

func TestRun_CancelledRunDispatchesNothingMore(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{responses: []string{
		batchResponse(call{"get_disk_info", `{}`}, call{"list_directory", `{"path":"/mnt"}`}),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelled atomic.Bool
	stub := &mediaAgentStub{onHit: func() {
		cancelled.Store(true)
		cancel()
	}}
	ag, database := newRunTestAgent(t, llm, withMediaAgent(t, stub))
	inc := seedRunIncident(t, database)

	_, conv, _ := runOnce(ctx, ag, inc)
	if !cancelled.Load() {
		t.Fatal("the first call never reached the media agent")
	}
	if hits := stub.hits(); len(hits) != 1 {
		t.Errorf("media agent got %v, want only the first call", hits)
	}
	if missing := unpaired(conv); len(missing) != 0 {
		t.Errorf("returned conversation leaves %v unanswered", missing)
	}
	if got := toolReply(conv, "call_xlist_directory"); !strings.Contains(got, "not run: the run was stopped") {
		t.Errorf("list_directory reply = %q, want not run", got)
	}
}
