package agent

import (
	"context"
	"errors"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
)

type Verdict string

const (
	GateAllowed       Verdict = "allowed"
	GateTripped       Verdict = "tripped"
	GateDiagnosisOnly Verdict = "diagnosis_only"
	GatePaused        Verdict = "paused"
	GateSuperseded    Verdict = "superseded"
)

type Gate interface {
	Claim(ctx context.Context) error
	Act(ctx context.Context, tool string) (Verdict, error)
	Live(ctx context.Context) Verdict
}

const (
	notRunDiagnosisOnly = "not run: budget spent, diagnosis only"
	notRunUnrecorded    = "not run: could not record the action"
)

func gateStop(v Verdict) (runEventKind, bool) {
	switch v {
	case GatePaused:
		return evPaused, true
	case GateSuperseded:
		return evStale, true
	case GateAllowed, GateTripped, GateDiagnosisOnly:
	}
	return "", false
}

func claimEvent(err error) runEvent {
	if errors.Is(err, ErrIncidentNotInvestigatable) {
		return runEvent{kind: evClaimRefused, err: err}
	}
	return runEvent{kind: evClaimFailed, err: err}
}

func (a *Agent) act(
	ctx context.Context, gate Gate, call openai.ToolCall, messages *[]openai.ChatCompletionMessage,
) (runEvent, bool) {
	fn := call.Function.Name
	v, err := gate.Act(ctx, fn)
	reply := func(text string) (runEvent, bool) {
		*messages = append(*messages, openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleTool, Content: jsonResult(map[string]any{keyError: text}), ToolCallID: call.ID,
		})
		return runEvent{}, true
	}
	if err != nil {
		a.log.ErrorContext(ctx, "record autonomous action", "tool", fn, "error", err)
		return reply(notRunUnrecorded)
	}
	switch v {
	case GateAllowed:
		return runEvent{}, false
	case GateTripped:
		return runEvent{kind: evBudgetTripped}, false
	case GateDiagnosisOnly:
		return reply(notRunDiagnosisOnly)
	case GatePaused, GateSuperseded:
	}
	kind, _ := gateStop(v)
	return runEvent{kind: kind}, false
}

func (a *Agent) conclude(
	ctx context.Context, inc *db.Incident, preFix *FixSignature, call openai.ToolCall,
	messages *[]openai.ChatCompletionMessage,
) (runEvent, *DiagnosticResult) {
	result, err := a.handleCompleteDiagnosis(ctx, inc, preFix, call.Function.Arguments)
	if err != nil {
		return runEvent{kind: evBatchFailed, err: err}, nil
	}
	*messages = append(*messages, openai.ChatCompletionMessage{
		Role:       openai.ChatMessageRoleTool,
		Content:    jsonResult(map[string]any{keyStatus: "recorded"}),
		ToolCallID: call.ID,
	})
	return runEvent{kind: evConcluded}, result
}
