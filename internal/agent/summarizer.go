package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

const summaryPrompt = `You are summarizing an interrupted media diagnostic session for hand-off to a fresh agent run.

Produce a concise fact sheet in plain text covering:
- What the incident was (title, problem type, Jellyfin item ID if known)
- What was discovered (file paths, error codes, PlaybackInfo results, disk info, torrent state)
- What autonomous actions were taken and their outcomes
- What is still unresolved or uncertain and should be tried next

Be specific — include exact paths, error codes, and tool results. Omit conversational filler.`

// Summarizer condenses a stored conversation into a briefing for a new agent run.
type Summarizer struct {
	llm   *openai.Client
	model string
}

func NewSummarizer(llm *openai.Client, model string) *Summarizer {
	return &Summarizer{llm: llm, model: model}
}

// Summarize converts a stored JSON conversation into a plain-text fact sheet.
// Returns ("", nil) when rawConversation is empty.
func (s *Summarizer) Summarize(ctx context.Context, rawConversation json.RawMessage) (string, error) {
	if len(rawConversation) == 0 {
		return "", nil
	}

	var messages []openai.ChatCompletionMessage
	if err := json.Unmarshal(rawConversation, &messages); err != nil {
		return "", fmt.Errorf("unmarshal conversation: %w", err)
	}

	var sb strings.Builder
	for _, m := range messages {
		if m.Role == openai.ChatMessageRoleSystem {
			continue
		}
		fmt.Fprintf(&sb, "%s: ", m.Role)
		if m.Content != "" {
			sb.WriteString(m.Content)
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, " [tool_call %s(%s)]", tc.Function.Name, tc.Function.Arguments)
		}
		sb.WriteString("\n")
	}

	resp, err := s.llm.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: summaryPrompt},
			{Role: openai.ChatMessageRoleUser, Content: sb.String()},
		},
	})
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}
	return resp.Choices[0].Message.Content, nil
}
