package incident_test

import (
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/incident"
)

func TestF13_SentVerbsCoverEveryCardButton(t *testing.T) {
	t.Parallel()
	sent := incident.EscalationVerbsForTest()
	for _, verb := range incident.CardButtonVerbsForTest() {
		if !slices.Contains(sent, verb) {
			t.Errorf("verb %q is rendered but never in the sent verbs, so Discord drops it", verb)
		}
	}
}
