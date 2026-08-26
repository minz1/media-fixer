package agent

import (
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
