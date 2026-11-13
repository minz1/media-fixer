package agent_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/journal"
)

func TestRS5_DesyncSearchRefusedAfterOtherReport(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		incident string
		params   map[string]any
		refused  bool
	}{
		{"other report, same target, other title", "older",
			map[string]any{"title": "the show (us)", "target": "ep:42", "language": "english"}, true},
		{"other report, other episode", "older",
			map[string]any{"title": "Show", "target": "ep:43", "language": "en"}, false},
		{"other report, other language", "older",
			map[string]any{"title": "Show", "target": "ep:42", "language": "fr"}, false},
		{"this report", "inc", map[string]any{"title": "Show", "target": "ep:42", "language": "en"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := &bazarrFake{subs: func(int) (int, string) { return http.StatusOK, subsBody(oldSub, newSub) }}
			disp, _ := newBazarrDisp(t, f, &agent.BazarrWaits{})
			disp.Attempts = &fakeAttempts{list: []journal.Attempt{
				{
					IncidentID: c.incident,
					Tool:       "bazarr_search_subtitles",
					At:         time.Now().Add(-time.Hour),
					Params:     c.params,
				},
			}}
			_, err := disp.CallDesyncSearchForTest(t.Context(), "inc", time.Millisecond, 20*time.Millisecond,
				searchArgs("en"))
			if (err != nil) != c.refused {
				t.Fatalf("err = %v, want refused %v", err, c.refused)
			}
			if c.refused && f.patchCount() != 0 {
				t.Errorf("refused search still patched %d times", f.patchCount())
			}
		})
	}
}
