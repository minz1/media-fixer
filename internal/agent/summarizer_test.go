package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
)

func summarizerFor(t *testing.T, handler http.HandlerFunc) *agent.Summarizer {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := openai.DefaultConfig("k")
	cfg.BaseURL = srv.URL
	return agent.NewSummarizer(openai.NewClientWithConfig(cfg), "test-model")
}

func TestSummarizerComplete(t *testing.T) {
	t.Parallel()
	var got openai.ChatCompletionRequest
	s := summarizerFor(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`))
	})
	out, err := s.Complete(context.Background(), "sys text", "user text")
	if err != nil || out != "answer" {
		t.Fatalf("got %q, %v", out, err)
	}
	if got.Model != "test-model" || len(got.Messages) != 2 ||
		got.Messages[0].Role != openai.ChatMessageRoleSystem || got.Messages[0].Content != "sys text" ||
		got.Messages[1].Role != openai.ChatMessageRoleUser || got.Messages[1].Content != "user text" {
		t.Errorf("request = %+v", got)
	}
}

func TestSummarizerComplete_NoChoices(t *testing.T) {
	t.Parallel()
	s := summarizerFor(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"choices":[]}`)) })
	if _, err := s.Complete(context.Background(), "a", "b"); err == nil {
		t.Fatal("want an error for empty choices")
	}
}

func TestSummarize_UsesSummaryPrompt(t *testing.T) {
	t.Parallel()
	var got openai.ChatCompletionRequest
	s := summarizerFor(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"sheet"}}]}`))
	})
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	out, err := s.Summarize(context.Background(), msgs)
	if err != nil || out != "sheet" {
		t.Fatalf("got %q, %v", out, err)
	}
	if len(got.Messages) != 2 || got.Messages[1].Content != "user: hi\n" ||
		got.Messages[0].Content == "" {
		t.Errorf("messages = %+v", got.Messages)
	}
}
