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
	Act(ctx context.Context, tool string, params map[string]any) (Verdict, error)
	Gen() int64
	Live(ctx context.Context) Verdict
	Calls() []CallRecord
	Record(CallRecord)
}

type CallRecord struct {
	Tool    string
	Params  map[string]any
	Status  callOutcome
	Err     string
	Counted bool
}

const (
	notRunDiagnosisOnly = "not run: budget spent, diagnosis only"
	notRunUnrecorded    = "not run: could not record the action"
	notRunTripped       = "not run: autonomous action limit reached"
	notRunPaused        = "not run: autonomous actions paused"
	notRunSuperseded    = "not run: the run was stopped"
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
