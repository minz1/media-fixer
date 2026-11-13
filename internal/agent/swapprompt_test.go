package agent_test

import (
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
)

func TestD9_PromptSwap(t *testing.T) {
	t.Parallel()
	prompt := agent.SystemPromptForTest()
	for _, want := range []string{
		"Use sonarr_swap_plan only after list_alternatives lists a cached alternative and stream_alignment showed the current file's subtitle track broken or absent",
		"complete with escalate_action=swap_release, escalate_params from the plan (its download_id) and requires_approval=true",
		"Set escalate_params: title, season, episode, download_id (the download_id",
		"Never swap for a missing language before bazarr_search_subtitles has run",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}
