package stuckqueue_test

import (
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func stateRec(state client.FileState) client.QueueEntry {
	r := stuckRec(1, "D", 1, false, notUpgrade)
	r.File = state
	return r
}

func TestQ6a_ReadersAgree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rec     client.QueueEntry
		maybe   bool
		verdict stuckqueue.Verdict
		reason  string
	}{
		{"unknown", stateRec(client.FileUnknown), true, stuckqueue.VerdictGap, "can't tell if it has a file"},
		{"missing", stateRec(client.FileMissing), false, stuckqueue.VerdictGap, "episode missing (S01E01)"},
		{
			"present", stateRec(client.FilePresent), true, stuckqueue.VerdictClear,
			"Sonarr has a file for S01E01 but didn't compare it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := stuckqueue.Group(stuckqueue.AppSonarr, []client.QueueEntry{tc.rec})[0]
			if got := d.MaybeHasFile(); got != tc.maybe {
				t.Errorf("MaybeHasFile = %v, want %v", got, tc.maybe)
			}
			if got := stuckqueue.Classify(d); got != tc.verdict {
				t.Errorf("Classify = %s, want %s", got, tc.verdict)
			}
			it := stuckqueue.Item{Download: d, Verdict: stuckqueue.VerdictGap}
			c := stuckqueue.StuckCardForTest([]stuckqueue.Item{it}, false)
			if len(c.Items) != 1 || !strings.Contains(c.Items[0].Text, tc.reason) {
				t.Errorf("card = %+v, want reason %q", c.Items, tc.reason)
			}
		})
	}
}
