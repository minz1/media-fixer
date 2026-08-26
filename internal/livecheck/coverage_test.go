package livecheck_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/livecheck"
)

// TestCheckRegistry_CoversEveryTool is the build-time guarantee this whole
// package exists to provide: every tool the agent can dispatch — including
// owner-approval-gated ones, which still need a (dry-run) live check — has a
// registered check here. Adding a tool to the agent's registry without
// adding a corresponding check fails this test.
func TestCheckRegistry_CoversEveryTool(t *testing.T) {
	t.Parallel()

	checked := make(map[string]bool)
	for _, spec := range livecheck.CheckRegistryForTest() {
		checked[spec.Tool] = true
	}

	for _, name := range agent.ToolNames() {
		if agent.ExcludedFromLiveCheck(name) {
			continue
		}
		if !checked[name] {
			t.Errorf("tool %q has no registered live-check", name)
		}
	}
}

// TestCheckRegistry_NoStaleEntries is the mirror check: a checkSpec for a
// tool that no longer exists in the agent's registry would run and always
// fail with "unknown tool", masking real problems in the report.
func TestCheckRegistry_NoStaleEntries(t *testing.T) {
	t.Parallel()

	toolNames := make(map[string]bool)
	for _, name := range agent.ToolNames() {
		toolNames[name] = true
	}

	for _, spec := range livecheck.CheckRegistryForTest() {
		if !toolNames[spec.Tool] {
			t.Errorf("check registered for %q, which is not in agent.ToolNames()", spec.Tool)
		}
	}
}

// TestCheckRegistry_UniqueTools catches a duplicate checkSpec entry, which
// would silently make one check's result overwrite when reasoning about
// coverage (both would appear in the report, but coverage tests only see
// the tool name, not the duplication).
func TestCheckRegistry_UniqueTools(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for _, spec := range livecheck.CheckRegistryForTest() {
		if seen[spec.Tool] {
			t.Errorf("duplicate check registered for tool %q", spec.Tool)
		}
		seen[spec.Tool] = true
	}
}
