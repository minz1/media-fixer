package stuckread_test

import (
	"context"
	"errors"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
)

func exitOf(result *agent.DiagnosticResult, conv []openai.ChatCompletionMessage, err error) agent.RunExit {
	switch {
	case errors.Is(err, context.Canceled):
		return agent.RunExit{State: agent.RunSuperseded, Conversation: conv, Err: err}
	case errors.Is(err, agent.ErrIncidentNotInvestigatable):
		return agent.RunExit{State: agent.RunNotClaimed, Err: err}
	case err != nil:
		return agent.RunExit{State: agent.RunLLMFailed, Conversation: conv, Err: err}
	case result == nil:
		return agent.RunExit{State: agent.RunSuperseded, Conversation: conv, Err: context.Canceled}
	}
	return agent.RunExit{State: agent.RunConcluded, Finding: result, Conversation: conv}
}
