package agent

import (
	"context"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

// Exported for internal/agent/registry_test.go (package agent_test).

// ToolSpec re-exports toolSpec; its fields (Name, Def, Risk, Handler) are
// already capitalized, so only the type name itself needs exporting.
type ToolSpec = toolSpec

func ToolRegistry() []ToolSpec            { return toolRegistry() }
func ToolDefsForTest() []openai.Tool      { return toolDefs() }
func SystemPromptForTest() string         { return systemPrompt }
func EscalateActionEnumForTest() []string { return escalateActionEnum() }

func LokiFilterRegexForTest(terms []string) string { return lokiFilterRegex(terms) }

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
	return a.actionAlreadyApplied(ctx, incidentID, 0, action, argsJSON)
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

type (
	CallState   = callState
	CallKind    = callKind
	CallEffect  = callEffect
	CallSnap    = callSnap
	CallOutcome = callOutcome
)

const (
	CallProposed       = callProposed
	CallScreened       = callScreened
	CallActed          = callActed
	CallBlockedApplied = callBlockedApplied
	CallBlockedUnknown = callBlockedUnknown
	CallBlockedRepeat  = callBlockedRepeat
	CallNotRun         = callNotRun
	CallRefused        = callRefused
	CallApplied        = callApplied
	CallFailed         = callFailed
	CallInterrupted    = callInterrupted

	OutcomeApplied     = outcomeApplied
	OutcomeFailed      = outcomeFailed
	OutcomeRefused     = outcomeRefused
	OutcomeInterrupted = outcomeInterrupted
	OutcomeInvalid     = outcomeInvalid

	EvLiveDenied         = evLiveDenied
	EvAppliedBefore      = evAppliedBefore
	EvUnknownBefore      = evUnknownBefore
	EvSeenInRun          = evSeenInRun
	EvCleared            = evCleared
	EvRefusal            = evRefusal
	EvPreconditionFailed = evPreconditionFailed
	EvGateDenied         = evGateDenied
	EvGateFailed         = evGateFailed
	EvGateAllowed        = evGateAllowed
	EvSucceeded          = evSucceeded
	EvNotReady           = evNotReady
	EvErrored            = evErrored
	EvCut                = evCut

	EffReply    = effReply
	EffLog      = effLog
	EffRemember = effRemember
	EffVerdict  = effVerdict
)

func CallStates() []CallState { return callStates() }

func CallKinds() []CallKind { return callKinds() }

func CallOutcomes() []CallOutcome { return callOutcomes() }

func CallMeta(s CallState) callStateMeta { return callMeta(s) }

func NextCall(cur CallSnap, k CallKind) (CallSnap, []CallEffect, error) { return nextCall(cur, k) }

func CheckCall(s CallSnap) error { return checkCall(s) }

func CallMatches(s CallSnap, k CallKind) int { return callMatches(s, k) }

type ToolImpactForTest = toolImpact

func (d *Dispatcher) CallScoped(ctx context.Context, s Scope, name string, args map[string]any) (any, error) {
	return d.call(ctx, &toolCall{scope: s}, name, args)
}

func (d *Dispatcher) CallGated(ctx context.Context, s Scope, g Gate, name string, args map[string]any) (any, error) {
	return d.call(ctx, &toolCall{scope: s, gate: g}, name, args)
}

func (a *Agent) SetReadinessForTest(timeout, interval time.Duration) {
	a.readyTimeout, a.readyInterval = timeout, interval
}

func (a *Agent) SetSideEffectBoundForTest(d time.Duration) { a.sideBound = d }

func BoundForTest(s ToolSpec) time.Duration { return s.bound() }

const JournalWriteTimeoutForTest = journalWriteTimeout

func (a *Agent) RefuseForTest(ctx context.Context, inc *db.Incident, tool, reason string, g Gate) string {
	spec, _ := specByName(tool)
	v := &invocation{
		a: a, ctx: ctx, fn: tool,
		snap: callSnap{State: callProposed, Impact: spec.Impact},
		c:    &toolCall{scope: Scope{IncidentID: inc.ID, What: inc.What, ReportedAt: inc.CreatedAt}, gate: g},
	}
	v.step(evCleared, stepInfo{})
	v.beforeAct(nil, refuse(reason))
	return v.reply
}

func ManualFindingForTest(s RunState, err error) *DiagnosticResult { return manualFinding(s, err) }

func (a *Agent) FetchQueueForTest(ctx context.Context, mediaType string) ([]client.QueueRecord, error) {
	return a.fetchQueue(ctx, mediaType)
}

func AlignStreamsForTest(file []mediaagentapi.ProbeStream, jf []client.MediaStream, wantISO2 string) Alignment {
	return alignStreams(file, jf, wantISO2)
}

func LangCodesForTest(s string) (string, string, bool) { return langCodes(s) }

func DetectLanguageForTest(text string) string { return detectLanguage(text) }

func CueLinesForTest(srt string, n int) []string { return cueLines(srt, n) }

func (a *Agent) ConcludeForTest(
	ctx context.Context, inc *db.Incident, call openai.ToolCall, gate Gate, messages *[]openai.ChatCompletionMessage,
) (*DiagnosticResult, error) {
	ev, result := a.conclude(ctx, inc, nil, call, gate, messages)
	return result, ev.err
}

func AlternativesForTest(events []client.SeriesEvent, season, episode int, cached map[string]bool) []Alternative {
	return alternatives(events, season, episode, cached)
}

func IsAutonomousActionForTest(tool string) bool { return isAutonomousAction(tool) }

func (d *Dispatcher) CallPolledForTest(
	ctx context.Context, every, total time.Duration, name string, args map[string]any,
) (any, error) {
	return d.call(ctx, &toolCall{pollEvery: every, pollFor: total}, name, args)
}

func (d *Dispatcher) CallScopedPolledForTest(
	ctx context.Context, what string, g Gate, every, total time.Duration, name string, args map[string]any,
) (any, error) {
	return d.call(ctx, &toolCall{scope: Scope{What: what}, gate: g, pollEvery: every, pollFor: total}, name, args)
}

func LastBazarrResyncForTest(m []openai.ChatCompletionMessage) *BazarrResyncResult {
	return lastBazarrResync(m)
}
