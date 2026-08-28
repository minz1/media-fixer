package agent

import (
	"context"
	"time"

	openai "github.com/sashabaranov/go-openai"
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
