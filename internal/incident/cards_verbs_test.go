package incident

import (
	"slices"
	"testing"
)

func TestF13_SentVerbsCoverEveryCardButton(t *testing.T) {
	sent := escalationVerbs()
	for _, sp := range cardSpecs(cardPlan{}, nil) {
		if !slices.Contains(sent, sp.verb) {
			t.Errorf("button %q (verb %q) is rendered but never in the sent verbs, so Discord drops it", sp.label, sp.verb)
		}
	}
}
