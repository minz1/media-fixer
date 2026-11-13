package agent_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
)

func TestTorrentReaddRetired_NoToolEscalationOrPromptText(t *testing.T) {
	t.Parallel()
	if _, ok := agent.SpecByNameForTest("decypharr_delete_readd"); ok {
		t.Error("registry still has decypharr_delete_readd")
	}
	if slices.Contains(agent.EscalateActionEnumForTest(), "delete_torrent_readd") {
		t.Error("escalate_action enum still offers delete_torrent_readd")
	}
	prompt := strings.ToLower(agent.SystemPromptForTest())
	for _, gone := range []string{"readd", "re-add it from its magnet"} {
		if strings.Contains(prompt, gone) {
			t.Errorf("system prompt still mentions %q", gone)
		}
	}
}

func TestTorrentReaddRetired_DeadTorrentRoutesToRefreshLinks(t *testing.T) {
	t.Parallel()
	spec, ok := agent.SpecByNameForTest("refresh_decypharr_links")
	if !ok {
		t.Fatal("registry has no refresh_decypharr_links")
	}
	if !strings.Contains(spec.Def.Description, "re-adds") {
		t.Errorf("refresh_decypharr_links description does not say decypharr re-adds a dead torrent: %q",
			spec.Def.Description)
	}
	prompt := agent.SystemPromptForTest()
	if !strings.Contains(prompt, "decypharr re-adds it from its magnet itself") {
		t.Error("system prompt does not route a dead torrent to refresh_decypharr_links")
	}
}
