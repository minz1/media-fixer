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

// extractJSONObject pulls the first {...} block out of a model reply,
// tolerating the ```json fences and short preambles models wrap JSON in even
// when told not to. Returns the input trimmed if no object is found, so the
// caller's own decode produces the error.
//
// This matters more than it looks: on a parse failure Service.evaluateDiagnosis
// proceeds with the action anyway, and the three conditions that route a
// diagnosis to review in the first place (low confidence, repeating a failed
// action, several actions already tried) are exactly the ones you least want
// auto-approved because of a markdown fence.
func extractJSONObject(content string) string {
	trimmed := strings.TrimSpace(content)
	if fenced := strings.Index(trimmed, "```"); fenced >= 0 {
		rest := trimmed[fenced+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			rest = rest[:end]
		}
		trimmed = strings.TrimSpace(rest)
	}
	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return trimmed
	}
	return trimmed[start : end+1]
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

// controlSystemPrompt covers two distinct reasons a diagnosis reaches review,
// spelled out in the per-call user message: the agent explicitly flagged its
// own proposal as needing owner approval, or the system's own heuristics
// flagged an otherwise-autonomous proposal as risky (it repeats an action
// already tried and failed on this incident, or the agent's own confidence
// is low). "approve" means different things downstream for each — surface to
// the owner for the first, let the fix proceed autonomously for the second —
// but the reviewer doesn't need to know that; it only judges whether what's
// proposed is reasonable given the evidence.
const controlSystemPrompt = `You are a control reviewer for an automated media-stack diagnostic agent.
The diagnostic agent has examined a playback incident and reached a conclusion that a second
opinion is being requested on before it proceeds. This happens for one of two reasons, stated in
the message below: either the agent itself flagged the action as needing owner approval, or the
system flagged an otherwise-autonomous action as risky (e.g. it repeats something already tried
and failed on this incident, or the agent's own confidence is low).

Your job is to review the agent's reasoning and return one of three verdicts:

- "approve": The agent's proposal is reasonable as stated — let it proceed the way the agent
  intended (whether that means owner approval or immediate autonomous action).
- "suggest_alternative": The agent missed a less destructive fix, or is about to repeat something
  that already failed without accounting for that. Provide the alternative.
- "escalate_to_owner": Something looks wrong with the diagnosis (contradictory evidence,
  hallucinated path, repeating a failed action without acknowledging it, etc.), regardless of
  which of the two reasons triggered this review. Notify the owner with a flag that the diagnosis
  may be unreliable.

Respond with ONLY valid JSON matching this schema — no prose, no markdown:
{
  "verdict": "approve" | "suggest_alternative" | "escalate_to_owner",
  "reason": "<concise explanation>",
  "alternative_action": "<only when verdict is suggest_alternative, otherwise omit>"
}`

// Review runs a single LLM completion reviewing the diagnostic conversation.
// conversation is the full message history from Agent.Run(); proposal
// describes what the agent concluded and why this review was triggered (see
// controlSystemPrompt) — either "wants to escalate X for owner approval", or
// "proposes to autonomously apply X, but review was triggered because Y".
func (r *ControlReviewer) Review(
	ctx context.Context,
	conversation []openai.ChatCompletionMessage,
	proposal string,
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
			"%s\n\nPlease review and return your verdict as JSON.",
			proposal,
		),
	})

	resp, err := r.llm.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:    r.model,
		Messages: messages,
		// The prompt asks for bare JSON; this makes the provider enforce it
		// rather than leaving it to the model's goodwill. Combined with
		// extractJSONObject below, a fenced or prose-wrapped reply no longer
		// fails the review outright.
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("control review llm: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, errNoChoices
	}
	raw := extractJSONObject(resp.Choices[0].Message.Content)
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
