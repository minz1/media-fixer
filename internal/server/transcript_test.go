package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/server"
)

func TestSummarizeArgs(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	tests := []struct{ name, in, want string }{
		{"pairs sorted", `{"path": "/dl/a", "series_id": 4}`, "path=/dl/a series_id=4"},
		{"empty object", `{}`, ""},
		{"raw on parse failure", "not json\n at all", "not json at all"},
		{"nested value", `{"ids": [1,2]}`, "ids=[1,2]"},
	}
	for _, tt := range tests {
		if got := server.SummarizeArgs(tt.in); got != tt.want {
			t.Errorf("%s: server.SummarizeArgs(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
	got := server.SummarizeArgs(`{"p": "` + long + `"}`)
	if r := []rune(got); len(r) != 80 || !strings.HasSuffix(got, "…") {
		t.Errorf("long args not truncated to %d runes with ellipsis: %q (%d)", 80, got, len(r))
	}
}

func TestFirstLine(t *testing.T) {
	t.Parallel()
	if got := server.FirstLine("\n  \nhello\nworld", 10); got != "hello" {
		t.Errorf("got %q", got)
	}
	if got := server.FirstLine("abcdefghij", 5); got != "abcd"+"…" {
		t.Errorf("got %q", got)
	}
	if got := server.FirstLine("{\n  \"a\": 1,\n  \"b\": [2]\n}", 40); got != `{"a":1,"b":[2]}` {
		t.Errorf("json preview = %q, want compact json", got)
	}
	if got := server.FirstLine("", 5); got != "" {
		t.Errorf("got %q", got)
	}
}

func transcriptFixture(rounds int) []openai.ChatCompletionMessage {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "report"},
	}
	for r := 1; r <= rounds; r++ {
		id := string(rune('a' + r))
		msgs = append(msgs,
			openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{{
					ID: id, Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{Name: "t", Arguments: `{"n": 1}`},
				}},
			},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: "out\nmore"},
		)
	}
	return append(msgs, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "done"})
}

func TestBuildTranscriptTurns_SummariesAndConclusion(t *testing.T) {
	t.Parallel()
	turns, _ := server.CondensedTurnsForTest(transcriptFixture(2), true)
	if len(turns) != 5 {
		t.Fatalf("got %d turns, want 5", len(turns))
	}
	tc := turns[2].Tools[0]
	if tc.ArgsLine != "n=1" || tc.ResultTop != "out" {
		t.Errorf("tool summary = %q / %q", tc.ArgsLine, tc.ResultTop)
	}
	for i, tr := range turns {
		if want := i == 4; tr.Final != want {
			t.Errorf("turn %d Final = %v, want %v", i, tr.Final, want)
		}
	}
}

func TestBuildTranscriptTurns_NoConclusionWhileLive(t *testing.T) {
	t.Parallel()
	for i, tr := range server.LiveTurnsForTest(transcriptFixture(2)) {
		if tr.Final {
			t.Errorf("turn %d marked as the conclusion while the agent is still working", i)
		}
	}
}

func TestCondenseTurns(t *testing.T) {
	t.Parallel()
	msgs := transcriptFixture(4)

	all, hidden := server.CondensedTurnsForTest(msgs, true)
	if len(all) != 7 || hidden != 0 {
		t.Errorf("full: %d turns, %d hidden", len(all), hidden)
	}

	got, hidden := server.CondensedTurnsForTest(msgs, false)
	if hidden != 2 || len(got) != 3 || got[0].Round != 3 {
		t.Errorf("condensed: %d turns, %d hidden, first round %d", len(got), hidden, got[0].Round)
	}

	short, hidden := server.CondensedTurnsForTest(transcriptFixture(1), false)
	if hidden != 0 || len(short) != 3 || short[0].Role != openai.ChatMessageRoleUser {
		t.Errorf("short: %d turns, %d hidden", len(short), hidden)
	}
}

func TestD9_8_TranscriptMarksTheRunsStepCount(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	ctx := context.Background()
	inc := &db.Incident{
		Status: db.StatusInvestigating, Source: "discord", ReportedBy: "t", What: "cant_play", Title: "T",
	}
	lifecycletest.Incident(t, database, inc)
	j := journal.New(database)
	seed := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "report"},
	}
	page := func() string {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID, nil))
		return rec.Body.String()
	}

	if err := j.RunStarted(ctx, inc.ID, seed); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 2; round++ {
		msg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "thinking"}
		if err := j.LLMRound(ctx, inc.ID, round, msg, nil); err != nil {
			t.Fatal(err)
		}
	}
	if body := page(); !strings.Contains(body, `id="incident-transcript" data-turns="4"`) {
		t.Errorf("first run: transcript doesn't carry its step count")
	}

	if err := j.RunStarted(ctx, inc.ID, seed); err != nil {
		t.Fatal(err)
	}
	if body := page(); !strings.Contains(body, `id="incident-transcript" data-turns="2"`) {
		t.Errorf("rerun: transcript step count didn't drop to the new run's")
	}
}

func TestL7_HighlightResetsOnNewRunWithoutLowRender(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	ctx := context.Background()
	inc := &db.Incident{
		Status: db.StatusInvestigating, Source: "discord", ReportedBy: "t", What: "cant_play", Title: "T",
	}
	lifecycletest.Incident(t, database, inc)
	j := journal.New(database)
	seed := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleUser, Content: "report"},
	}
	runOf := func() string {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID, nil))
		m := regexp.MustCompile(`id="incident-transcript" data-turns="(\d+)" data-run="(\d+)"`).
			FindStringSubmatch(rec.Body.String())
		if m == nil {
			t.Fatal("transcript carries no run id")
		}
		return m[2]
	}
	round := func(n int) {
		msg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "thinking"}
		if err := j.LLMRound(ctx, inc.ID, n, msg, nil); err != nil {
			t.Fatal(err)
		}
	}

	if err := j.RunStarted(ctx, inc.ID, seed); err != nil {
		t.Fatal(err)
	}
	round(1)
	first := runOf()
	if err := j.RunStarted(ctx, inc.ID, seed); err != nil {
		t.Fatal(err)
	}
	round(1)
	round(2)
	if runOf() == first {
		t.Error("a new run keeps the old run id, so the highlight never resets")
	}
}
