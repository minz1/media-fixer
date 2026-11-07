package agent

import (
	"context"
	"slices"

	openai "github.com/sashabaranov/go-openai"
)

func answerPending(
	messages []openai.ChatCompletionMessage, calls []openai.ToolCall, reason string,
) []openai.ChatCompletionMessage {
	for _, c := range unansweredCalls(messages, calls) {
		messages = append(messages, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			Content:    jsonResult(map[string]any{keyError: "not run: " + reason}),
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

func closeBatch(
	ctx context.Context, round int, ev runEvent, calls []openai.ToolCall, messages []openai.ChatCompletionMessage,
) []openai.ChatCompletionMessage {
	ev.cancelled = ctx.Err() != nil
	snap := runSnapshot{State: RunInBatch, Started: true, Round: round, Replies: round + 1}
	next, _, err := nextRun(snap, ev)
	if err != nil {
		next, _ = failRun(snap, ev)
	}
	return answerPending(messages, calls, RunMeta(next.State).NotRun)
}
