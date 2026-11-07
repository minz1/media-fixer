package agent

import (
	"slices"

	openai "github.com/sashabaranov/go-openai"
)

func answerPending(
	messages []openai.ChatCompletionMessage, calls []openai.ToolCall, reason string,
) []openai.ChatCompletionMessage {
	for _, c := range unansweredCalls(messages, calls) {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			Content:    errorReply("not run: " + reason),
			ToolCallID: c.ID,
		})
	}
	return messages
}

func unansweredCalls(messages []openai.ChatCompletionMessage, calls []openai.ToolCall) []openai.ToolCall {
	start := 0
	for i, m := range slices.Backward(messages) {
		if m.Role == openai.ChatMessageRoleAssistant {
			start = i + 1
			break
		}
	}
	answered := map[string]bool{}
	for _, m := range messages[start:] {
		if m.Role == openai.ChatMessageRoleTool {
			answered[m.ToolCallID] = true
		}
	}
	var out []openai.ToolCall
	for _, c := range calls {
		if !answered[c.ID] {
			out = append(out, c)
		}
	}
	return out
}
