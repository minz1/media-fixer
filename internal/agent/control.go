package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

const (
	VerdictApprove            = "approve"
	VerdictSuggestAlternative = "suggest_alternative"
	VerdictEscalateToOwner    = "escalate_to_owner"
)

// ControlVerdict is the structured output from the control reviewer.
type ControlVerdict struct {
	Verdict           string `json:"verdict"`
	Reason            string `json:"reason"`
	AlternativeAction string `json:"alternative_action,omitempty"`
}

// ControlReviewer performs a single-shot review of a diagnostic agent run
// before surfacing approval-required escalations to the owner.
type ControlReviewer struct {
	llm   *openai.Client
	model string
	log   *slog.Logger
}

func NewControlReviewer(llm *openai.Client, model string, log *slog.Logger) *ControlReviewer {
	return &ControlReviewer{llm: llm, model: model, log: log}
}

const controlSystemPrompt = `You are a control reviewer for an automated media-stack diagnostic agent.
The diagnostic agent has examined a playback incident and concluded it cannot fix the problem
autonomously — it wants to escalate an action that requires owner approval.

Your job is to review the agent's reasoning and return one of three verdicts:

- "approve": The escalation is warranted. Surface to dashboard for owner action.
- "suggest_alternative": The agent missed a less destructive fix. Provide the alternative.
- "escalate_to_owner": Something looks wrong with the diagnosis (contradictory evidence,
  hallucinated path, etc.). Notify the owner with a flag that the diagnosis may be unreliable.

Respond with ONLY valid JSON matching this schema — no prose, no markdown:
{
  "verdict": "approve" | "suggest_alternative" | "escalate_to_owner",
  "reason": "<concise explanation>",
  "alternative_action": "<only when verdict is suggest_alternative, otherwise omit>"
}`

// Review runs a single LLM completion reviewing the diagnostic conversation.
// conversation is the full message history from Agent.Run(); escalation is
// the proposed escalate_action from the DiagnosticResult.
func (r *ControlReviewer) Review(
	ctx context.Context,
	conversation []openai.ChatCompletionMessage,
	escalation string,
) (*ControlVerdict, error) {
	const controlSeedExtra = 2
	messages := make([]openai.ChatCompletionMessage, 0, len(conversation)+controlSeedExtra)
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: controlSystemPrompt,
	})
	messages = append(messages, conversation...)
	messages = append(messages, openai.ChatCompletionMessage{
		Role: openai.ChatMessageRoleUser,
		Content: fmt.Sprintf(
			"The agent wants to escalate the following action for owner approval:\n\n%s\n\nPlease review and return your verdict as JSON.",
			escalation,
		),
	})

	resp, err := r.llm.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:    r.model,
		Messages: messages,
	})
	if err != nil {
		return nil, fmt.Errorf("control review llm: %w", err)
	}

	raw := strings.TrimSpace(resp.Choices[0].Message.Content)
	var verdict ControlVerdict
	if decodeErr := json.Unmarshal([]byte(raw), &verdict); decodeErr != nil {
		r.log.WarnContext(ctx, "control reviewer returned non-JSON", "raw", raw, "error", decodeErr)
		return nil, fmt.Errorf("control review parse: %w", decodeErr)
	}

	switch verdict.Verdict {
	case VerdictApprove, VerdictSuggestAlternative, VerdictEscalateToOwner:
	default:
		return nil, fmt.Errorf("control review: unexpected verdict %q", verdict.Verdict)
	}

	r.log.InfoContext(ctx, "control review complete", "verdict", verdict.Verdict, "reason", verdict.Reason)

	return &verdict, nil
}
