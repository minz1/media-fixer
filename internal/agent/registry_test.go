package agent_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
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

func TestRegistry_EveryWriteToolHasUserPhrase(t *testing.T) {
	t.Parallel()
	for _, spec := range agent.ToolRegistry() {
		if (spec.Risk == agent.RiskWrite || spec.Risk == agent.RiskApproval) && spec.UserPhrase == "" {
			t.Errorf("%s: empty UserPhrase", spec.Name)
		}
	}
}

func TestUserPhrase_Values(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"refresh_decypharr_links": "fixing the media storage",
		"decypharr_repair_sweep":  "fixing the media storage",
		"decypharr_cache_cleanup": "fixing the media storage",
		"decypharr_recheck":       "fixing the media storage",
		"restart_decypharr":       "fixing the media storage",
		"clear_jellyfin_cache":    "reloading the app",
		"restart_jellyfin":        "reloading the app",
		"jellyfin_library_scan":   "updating the library",
		"sonarr_rescan":           "updating the library",
		"radarr_rescan":           "updating the library",
		"arr_search_missing":      "looking for a new copy",
		"arr_remove_and_search":   "looking for a new copy",
		"remove_and_search":       "looking for a new copy",
		"nope":                    "",
	}
	for _, spec := range agent.ToolRegistry() {
		if spec.Risk == agent.RiskRead {
			want[spec.Name] = ""
			break
		}
	}
	for tool, phrase := range want {
		if got := agent.UserPhrase(tool); got != phrase {
			t.Errorf("UserPhrase(%q) = %q, want %q", tool, got, phrase)
		}
	}
}

func TestA6_ThreeListsOneRegistry(t *testing.T) {
	t.Parallel()
	wantDisruptive := []string{
		"restart_decypharr", "restart_jellyfin", "jellyfin_library_scan",
		"decypharr_cache_cleanup", "decypharr_repair_sweep", "refresh_decypharr_links",
	}
	var gotDisruptive []string
	for _, spec := range agent.ToolRegistry() {
		if agent.DisruptiveForTest(spec) {
			gotDisruptive = append(gotDisruptive, spec.Name)
		}
	}
	slices.Sort(wantDisruptive)
	slices.Sort(gotDisruptive)
	if !slices.Equal(gotDisruptive, wantDisruptive) {
		t.Errorf("disruptive = %v, want %v", gotDisruptive, wantDisruptive)
	}

	wantTier := map[string]agent.LiveTier{
		"jellyfin_search":         agent.LiveTierRead,
		"jellyfin_playback_info":  agent.LiveTierRead,
		"jellyfin_list_episodes":  agent.LiveTierRead,
		"jellyfin_scan_status":    agent.LiveTierRead,
		"dd_readability_test":     agent.LiveTierRead,
		"probe_streams":           agent.LiveTierRead,
		"stream_alignment":        agent.LiveTierRead,
		"list_alternatives":       agent.LiveTierRead,
		"subtitle_extract_test":   agent.LiveTierRead,
		"get_torrent_state":       agent.LiveTierRead,
		"loki_query":              agent.LiveTierRead,
		"list_directory":          agent.LiveTierRead,
		"get_disk_info":           agent.LiveTierRead,
		"get_repair_status":       agent.LiveTierRead,
		"get_repair_health":       agent.LiveTierRead,
		"arr_media_status":        agent.LiveTierRead,
		"arr_grab_history":        agent.LiveTierRead,
		"refresh_decypharr_links": agent.LiveTierWrite,
		"decypharr_repair_sweep":  agent.LiveTierWrite,
		"decypharr_cache_cleanup": agent.LiveTierWrite,
		"decypharr_recheck":       agent.LiveTierWrite,
		"sonarr_rescan":           agent.LiveTierWrite,
		"radarr_rescan":           agent.LiveTierWrite,
		"clear_jellyfin_cache":    agent.LiveTierWrite,
		"arr_search_missing":      agent.LiveTierWrite,
		"bazarr_search_subtitles": agent.LiveTierWrite,
		"bazarr_resync_subtitle":  agent.LiveTierWrite,
		"restart_decypharr":       agent.LiveTierDisruptive,
		"restart_jellyfin":        agent.LiveTierDisruptive,
		"jellyfin_library_scan":   agent.LiveTierDisruptive,
		"arr_remove_and_search":   agent.LiveTierPreview,
	}
	for name, want := range wantTier {
		got, ok := agent.LiveTierOf(name)
		if !ok || got != want {
			t.Errorf("LiveTierOf(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	if _, ok := agent.LiveTierOf("complete_diagnosis"); ok {
		t.Error("complete_diagnosis must have no live tier")
	}
	if len(wantTier) != len(agent.ToolNames())-1 {
		t.Errorf("table covers %d tools, registry has %d plus the excluded one", len(wantTier), len(agent.ToolNames()))
	}
}

func TestRegistry_WriteToolsDeclareImpact(t *testing.T) {
	t.Parallel()
	for _, spec := range agent.ToolRegistry() {
		isWrite := spec.Risk == agent.RiskWrite
		hasImpact := spec.Impact != agent.ImpactNone
		if isWrite != hasImpact {
			t.Errorf("%s: write = %v but impact = %v", spec.Name, isWrite, spec.Impact)
		}
	}
}

func TestLiveTier_TotalOverTools(t *testing.T) {
	t.Parallel()
	for _, name := range agent.ToolNames() {
		_, ok := agent.LiveTierOf(name)
		if ok == agent.ExcludedFromLiveCheck(name) {
			t.Errorf("%s: LiveTierOf ok = %v, excluded = %v", name, ok, agent.ExcludedFromLiveCheck(name))
		}
	}
	if _, ok := agent.LiveTierOf("no_such_tool"); ok {
		t.Error("unknown tool must have no live tier")
	}
}

const shutdownBudgetMirror = 60 * time.Second

func TestRegistry_SideEffectBoundsFitShutdown(t *testing.T) {
	t.Parallel()
	bounded := 0
	for _, spec := range agent.ToolRegistry() {
		bound := agent.BoundForTest(spec)
		if bound == 0 {
			continue
		}
		bounded++
		if bound+3*agent.JournalWriteTimeoutForTest > shutdownBudgetMirror-5*time.Second {
			t.Errorf("%s: bound %v + 3 journal writes exceeds the shutdown budget less 5s", spec.Name, bound)
		}
		if strings.HasPrefix(spec.Name, "restart_") && bound >= client.MediaAgentTimeout {
			t.Errorf("%s: bound %v is not under the media-agent timeout %v", spec.Name, bound, client.MediaAgentTimeout)
		}
	}
	if bounded != 13 {
		t.Errorf("%d tools have a side-effect bound, want the 13 write tools", bounded)
	}
}
