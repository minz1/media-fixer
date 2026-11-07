package agent

import (
	"context"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
)

// Exported for internal/agent/registry_test.go (package agent_test).

// ToolSpec re-exports toolSpec; its fields (Name, Def, Risk, Handler) are
// already capitalized, so only the type name itself needs exporting.
type ToolSpec = toolSpec

func ToolRegistry() []ToolSpec            { return toolRegistry() }
func ToolDefsForTest() []openai.Tool      { return toolDefs() }
func SystemPromptForTest() string         { return systemPrompt }
func EscalateActionEnumForTest() []string { return escalateActionEnum() }

func SpecByNameForTest(name string) (ToolSpec, bool) { return specByName(name) }

// Risk classification constants, re-exported for assertions in package agent_test.
const (
	RiskRead     = riskRead
	RiskWrite    = riskWrite
	RiskApproval = riskApproval
	RiskControl  = riskControl
)

// ToolArrRemoveAndSearchName is the arr_remove_and_search tool's registry name.
const ToolArrRemoveAndSearchName = toolArrRemoveAndSearch

// JSONSchemaTypeKey/JSONSchemaTypeObject let external tests validate a
// schema's "type": "object" pair without hardcoding a literal that would
// duplicate the one in jsonSchema/objectParam.
const (
	JSONSchemaTypeKey    = jsonSchemaTypeKey
	JSONSchemaTypeObject = jsonSchemaTypeObject
)

// WaitUntilReadyForTest exposes waitUntilReady for tools_test.go.
func WaitUntilReadyForTest(
	ctx context.Context, timeout, interval time.Duration, probe func(context.Context) error,
) error {
	return waitUntilReady(ctx, timeout, interval, probe)
}

// AttemptHistoryForTest exposes Agent.attemptHistory for agent_test.go.
func (a *Agent) AttemptHistoryForTest(ctx context.Context, incidentID string) string {
	return a.attemptHistory(ctx, incidentID)
}

// ActionAlreadyAppliedForTest exposes Agent.actionAlreadyApplied for agent_test.go.
func (a *Agent) ActionAlreadyAppliedForTest(ctx context.Context, incidentID, action, argsJSON string) bool {
	return a.actionAlreadyApplied(ctx, incidentID, action, argsJSON)
}

// CaptureSignatureForTest exposes Agent.captureSignature for agent_test.go.
func (a *Agent) CaptureSignatureForTest(ctx context.Context, itemID, title string) *FixSignature {
	return a.captureSignature(ctx, itemID, title)
}

// ImprovedForTest exposes the improved helper for agent_test.go.
func ImprovedForTest(pre, post *FixSignature) bool { return improved(pre, post) }

// DisruptionNoteForTest exposes Agent.disruptionNote for agent_test.go.
func (a *Agent) DisruptionNoteForTest(ctx context.Context, incidentID string) string {
	return a.disruptionNote(ctx, incidentID)
}

type (
	RunSnapshot  = runSnapshot
	RunEvent     = runEvent
	RunEventKind = runEventKind
	RunEffect    = runEffect
)

const (
	MaxRoundsForTest = maxRounds

	EvClaimed       = evClaimed
	EvClaimRefused  = evClaimRefused
	EvClaimFailed   = evClaimFailed
	EvToolCalls     = evToolCalls
	EvTextOnly      = evTextOnly
	EvLLMFailed     = evLLMFailed
	EvConcluded     = evConcluded
	EvBudgetTripped = evBudgetTripped
	EvBatchFailed   = evBatchFailed
	EvBatchDone     = evBatchDone
	EvPaused        = evPaused
	EvStale         = evStale

	EffRecordStarted = effRecordStarted
	EffAnswerPending = effAnswerPending
	EffRecordRound   = effRecordRound
	EffFinish        = effFinish
)

func NewRunEvent(kind RunEventKind, cancelled bool) RunEvent {
	return runEvent{kind: kind, cancelled: cancelled}
}

func (e runEvent) Kind() RunEventKind { return e.kind }

func (e runEvent) Cancelled() bool { return e.cancelled }

func RunStates() []RunState { return runStates() }

func RunEventKinds() []RunEventKind { return runEventKinds() }

func NextRun(cur RunSnapshot, ev RunEvent) (RunSnapshot, []RunEffect, error) { return nextRun(cur, ev) }

func CheckRun(s RunSnapshot) error { return checkRun(s) }

func RunMatches(s RunSnapshot, ev RunEvent) int { return runMatches(s, ev) }

func FailRun(cur RunSnapshot, ev RunEvent) (RunSnapshot, []RunEffect) { return failRun(cur, ev) }

func RunRowCountsForTest() (int, int) { return len(runAllowedRows()), len(runRejectRows()) }

func RejectReasonsForTest() []string {
	out := make([]string, 0, len(runRejectRows()))
	for _, r := range runRejectRows() {
		out = append(out, r.reject)
	}
	return out
}

func AnswerPendingForTest(
	messages []openai.ChatCompletionMessage, calls []openai.ToolCall, reason string,
) []openai.ChatCompletionMessage {
	return answerPending(messages, calls, reason)
}

func (a *Agent) DBForTest() *db.DB { return a.db }

const (
	ImpactNone   = impactNone
	ImpactItem   = impactItem
	ImpactShared = impactShared
	ImpactHeavy  = impactHeavy
)

func DisruptiveForTest(s ToolSpec) bool { return s.disruptive() }
