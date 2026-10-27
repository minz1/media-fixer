package agent_test

import (
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
)

func TestSystemPrompt_BufferingSectionLeadsWithReadSpeed(t *testing.T) {
	t.Parallel()
	prompt := agent.SystemPromptForTest()

	if !strings.Contains(prompt, "--- Playback problems (what=cant_play, buffering, missing_media) ---") {
		t.Error("playback protocol does not cover what=buffering")
	}
	const header = "--- Buffering / low quality (what=buffering) ---"
	_, after, found := strings.Cut(prompt, header)
	if !found {
		t.Fatalf("system prompt has no %q section", header)
	}
	section, _, _ := strings.Cut(after, "\n---")
	for _, want := range []string{"speed_mb_s", "dd_readability_test", "refresh_decypharr_links", "remove_and_search"} {
		if !strings.Contains(section, want) {
			t.Errorf("buffering section missing %q:\n%s", want, section)
		}
	}
}
