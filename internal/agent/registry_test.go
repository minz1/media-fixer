package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
)

// TestToolRegistry_UniqueNames catches a copy-paste duplicate tool entry —
// two specs with the same Name would let one silently shadow the other in
// Dispatcher.Call's lookup.
func TestToolRegistry_UniqueNames(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for _, spec := range agent.ToolRegistry() {
		if seen[spec.Name] {
			t.Errorf("duplicate tool name %q in registry", spec.Name)
		}
		seen[spec.Name] = true
	}
}

// TestToolRegistry_HandlersAndSchemas verifies every registry entry is
// actually dispatchable and describes a valid OpenAI function schema — the
// two properties toolDefs/Dispatcher.Call depend on but the compiler alone
// doesn't check (a tool's Parameters is stored as `any`, so a malformed
// schema wouldn't fail to compile).
func TestToolRegistry_HandlersAndSchemas(t *testing.T) {
	t.Parallel()
	for _, spec := range agent.ToolRegistry() {
		t.Run(spec.Name, func(t *testing.T) {
			t.Parallel()
			checkToolSpec(t, spec)
		})
	}
}

func checkToolSpec(t *testing.T, spec agent.ToolSpec) {
	t.Helper()
	if spec.Handler == nil {
		t.Fatalf("%s: nil Handler", spec.Name)
	}
	if spec.Def == nil {
		t.Fatalf("%s: nil Def", spec.Name)
	}
	if spec.Def.Name != spec.Name {
		t.Errorf("%s: Def.Name = %q, want match", spec.Name, spec.Def.Name)
	}
	if spec.Def.Description == "" {
		t.Errorf("%s: empty description", spec.Name)
	}
	checkToolSchema(t, spec)
}

func checkToolSchema(t *testing.T, spec agent.ToolSpec) {
	t.Helper()
	raw, isRawJSON := spec.Def.Parameters.(json.RawMessage)
	if !isRawJSON {
		t.Fatalf("%s: Parameters is %T, want json.RawMessage", spec.Name, spec.Def.Parameters)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("%s: parameters not valid JSON: %v", spec.Name, err)
	}
	if schema[agent.JSONSchemaTypeKey] != agent.JSONSchemaTypeObject {
		t.Errorf(
			"%s: schema type = %v, want %v",
			spec.Name, schema[agent.JSONSchemaTypeKey], agent.JSONSchemaTypeObject,
		)
	}
	if _, hasProperties := schema["properties"]; !hasProperties {
		t.Errorf("%s: schema missing properties", spec.Name)
	}
}

// TestToolDefs_ExcludesApprovalTools verifies toolDefs (what the LLM can
// call) is exactly the registry minus riskApproval entries — the invariant
// that keeps an owner-approval-only action from ever being offered to the
// LLM as a directly-callable tool.
func TestToolDefs_ExcludesApprovalTools(t *testing.T) {
	t.Parallel()
	defNames := make(map[string]bool)
	for _, d := range agent.ToolDefsForTest() {
		defNames[d.Function.Name] = true
	}

	for _, spec := range agent.ToolRegistry() {
		wantOffered := spec.Risk != agent.RiskApproval
		if defNames[spec.Name] != wantOffered {
			t.Errorf("tool %q: offered=%v, want %v", spec.Name, defNames[spec.Name], wantOffered)
		}
	}
}

// TestSystemPrompt_MentionsEveryCallableTool guards against the exact class
// of bug this project keeps hitting: a tool exists and works, but the prompt
// never tells the agent when/how to call it (or a rename leaves a stale name
// in the prompt). Read-only and autonomous-action tools must all appear by
// name; approval-gated and control tools are referenced differently (by
// escalate_action enum value, or implicitly) so are exempted.
func TestSystemPrompt_MentionsEveryCallableTool(t *testing.T) {
	t.Parallel()
	prompt := agent.SystemPromptForTest()
	for _, spec := range agent.ToolRegistry() {
		if spec.Risk == agent.RiskApproval || spec.Risk == agent.RiskControl {
			continue
		}
		if !strings.Contains(prompt, spec.Name) {
			t.Errorf("systemPrompt never mentions tool %q", spec.Name)
		}
	}
}

// TestEscalateActionEnum_RemoveAndSearchHasApprovalTool ties the
// escalate_action enum to a concrete registry entry: if the
// arr_remove_and_search tool were renamed or dropped from the registry
// without updating the enum (or vice versa), PlanEscalation/RunEscalation
// would silently fail at runtime instead of at build/test time.
func TestEscalateActionEnum_RemoveAndSearchHasApprovalTool(t *testing.T) {
	t.Parallel()
	spec, ok := agent.SpecByNameForTest(agent.ToolArrRemoveAndSearchName)
	if !ok {
		t.Fatalf("registry has no entry for %q", agent.ToolArrRemoveAndSearchName)
	}
	if spec.Risk != agent.RiskApproval {
		t.Errorf("%s: risk = %v, want RiskApproval", agent.ToolArrRemoveAndSearchName, spec.Risk)
	}

	found := false
	for _, v := range agent.EscalateActionEnumForTest() {
		if v == agent.EscalateRemoveAndSearch {
			found = true
		}
	}
	if !found {
		t.Errorf("escalateActionEnum() missing %q", agent.EscalateRemoveAndSearch)
	}
}
