package agent_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

// The tool-calling loop itself (Run / processToolCalls / llmCall) had no test
// coverage at all, which is why both the max-action escalation going silent
// and the unguarded resp.Choices[0] survived review. These tests drive the
// real Run against a scripted LLM endpoint.

// scriptedLLM serves one canned chat-completion body per request, in order,
// repeating the last one once exhausted.
type scriptedLLM struct {
	mu        sync.Mutex
	responses []string
	calls     int
}

func (s *scriptedLLM) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	body := s.responses[min(s.calls, len(s.responses)-1)]
	s.calls++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (s *scriptedLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// toolCallResponse renders a completion whose assistant message calls one tool.
func toolCallResponse(name, args string) string {
	msg := map[string]any{
		"role": "assistant",
		"tool_calls": []map[string]any{{
			"id":       "call_" + name,
			"type":     "function",
			"function": map[string]string{"name": name, "arguments": args},
		}},
	}
	b, _ := json.Marshal(map[string]any{
		"id": "c", "object": "chat.completion", "model": "stub",
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": "tool_calls"}},
	})
	return string(b)
}

// newRunTestAgent wires a real Agent against a scripted LLM and a decypharr
// stub that accepts every write, plus a real on-disk DB and journal.
func newRunTestAgent(t *testing.T, llm *scriptedLLM) (*agent.Agent, *db.DB) {
	t.Helper()

	llmSrv := httptest.NewServer(llm)
	t.Cleanup(llmSrv.Close)

	decypharr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(decypharr.Close)

	f, err := os.CreateTemp(t.TempDir(), "run-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	jrnl := journal.New(database)
	cfg := openai.DefaultConfig("stub-key")
	cfg.BaseURL = llmSrv.URL
	disp := &agent.Dispatcher{
		Decypharr: client.NewDecypharr(decypharr.URL, "tok"),
		DB:        database,
		Journal:   jrnl,
	}
	return agent.New(openai.NewClientWithConfig(cfg), "stub", disp, database, jrnl,
		slog.New(slog.DiscardHandler)), database
}

func seedRunIncident(t *testing.T, database *db.DB) *db.Incident {
	t.Helper()
	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "tester",
		What: "cant_play", Title: "Run Fixture",
	}
	if err := database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return inc
}

// TestRun_MaxAutonomousActionsLeavesEscalationToTheService is the regression
// test for the silent-escalation bug. On exceeding the action budget the agent
// used to write manual_test_needed itself; Service.escalateToOwner's
// TransitionStatus allow-list does not include manual_test_needed, so the
// escalation that followed transitioned nothing, logged "already escalated by
// another run" about a run that did not exist, and never called NotifyOwner —
// after three service-wide disruptions had already landed.
//
// The agent must therefore leave the incident in investigating and let the
// service own the transition.
func TestRun_MaxAutonomousActionsLeavesEscalationToTheService(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{responses: []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("decypharr_repair_sweep", `{}`),
		toolCallResponse("decypharr_cache_cleanup", `{}`),
		// The fourth distinct autonomous action trips the budget.
		toolCallResponse("decypharr_recheck", `{"name":"something"}`),
	}}
	ag, database := newRunTestAgent(t, llm)
	inc := seedRunIncident(t, database)
	ctx := context.Background()

	result, _, err := ag.Run(ctx, inc, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result == nil || !result.RequiresApproval {
		t.Fatalf("result = %+v, want a RequiresApproval result after the action budget is spent", result)
	}

	after, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status == db.StatusManualTestNeeded {
		t.Error("agent wrote manual_test_needed itself; Service.escalateToOwner will then " +
			"transition nothing and never notify the owner")
	}
	if after.Status != db.StatusInvestigating {
		t.Errorf("status = %q, want investigating so the service can escalate it", after.Status)
	}
	if !after.AutonomousLocked {
		t.Error("autonomous actions were not locked after the budget was spent")
	}
}

// TestRun_EmptyChoicesIsAnErrorNotAPanic pins the llmCall guard. A 200 with an
// empty choices array is a success to the HTTP client, so it used to reach
// resp.Choices[0] and panic in a bare goroutine with no recover — killing the
// whole process. OpenRouter returns exactly this shape for upstream provider
// errors, and Gemini does on a safety block.
func TestRun_EmptyChoicesIsAnErrorNotAPanic(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{responses: []string{
		`{"id":"c","object":"chat.completion","model":"stub","choices":[]}`,
	}}
	ag, database := newRunTestAgent(t, llm)
	inc := seedRunIncident(t, database)

	// A panic here fails the test by unwinding it, which is the point.
	_, _, err := ag.Run(context.Background(), inc, nil)
	if err == nil {
		t.Fatal("Run returned nil error for a response with no choices, want an error")
	}
	if llm.callCount() < 2 {
		t.Errorf("llm was called %d time(s); an empty-choices response should be retried "+
			"through the existing backoff before the run fails", llm.callCount())
	}
}

// arrOnlyDispatcher wires only Sonarr, so VerifyResolved has no Jellyfin item
// to probe — the shape every Discord-reported incident has, since only the
// Seerr path ever carries a Jellyfin item ID.
func arrOnlyDispatcher(t *testing.T, hasFile bool) *agent.Dispatcher {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "title": "Run Fixture"},
		})
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 10, "seasonNumber": 1, "episodeNumber": 1, "hasFile": hasFile, "monitored": true},
		})
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, _ *http.Request) {
		if !hasFile {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 10, "path": "/mnt/decypharr/run-fixture.mkv", "size": 1024},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
}

// TestVerifyResolved_WithoutJellyfinItemUsesArrFilePresence pins the fix for
// verification being structurally impossible on Discord-reported incidents.
// sourceOK and episodesOK both require Jellyfin data, so with no item ID
// VerifyResolved could never return true no matter what happened — every such
// incident burned its whole verification budget and then escalated as "fix
// applied but could not be verified", even when the fix had worked.
//
// *arr's confirmed file presence is the one ground-truth signal available
// without an item, and captureSignature already collects it.
func TestVerifyResolved_WithoutJellyfinItemUsesArrFilePresence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("arr has the file", func(t *testing.T) {
		t.Parallel()
		ag := agent.New(nil, "stub", arrOnlyDispatcher(t, true), nil, nil, slog.New(slog.DiscardHandler))
		if !ag.VerifyResolved(ctx, "", "Run Fixture", nil) {
			t.Error("VerifyResolved = false with no Jellyfin item but *arr reporting the file " +
				"present; Discord incidents can then never verify at all")
		}
	})

	t.Run("arr does not have the file", func(t *testing.T) {
		t.Parallel()
		ag := agent.New(nil, "stub", arrOnlyDispatcher(t, false), nil, nil, slog.New(slog.DiscardHandler))
		if ag.VerifyResolved(ctx, "", "Run Fixture", nil) {
			t.Error("VerifyResolved = true while *arr reports the file still missing")
		}
	})

	t.Run("no signal at all", func(t *testing.T) {
		t.Parallel()
		ag := agent.New(nil, "stub", &agent.Dispatcher{}, nil, nil, slog.New(slog.DiscardHandler))
		if ag.VerifyResolved(ctx, "", "Unknown Title", nil) {
			t.Error("VerifyResolved = true with no Jellyfin item and no *arr match; " +
				"absence of evidence is not evidence of a fix")
		}
	})
}
