package stuckqueue_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func TestStuckCard_CapsAtLimits(t *testing.T) {
	t.Parallel()
	var items []stuckqueue.Item
	for i := range 60 {
		items = append(items, stuckqueue.Item{
			App:     stuckqueue.AppSonarr,
			Title:   fmt.Sprintf("%-80s", fmt.Sprintf("Title %d", i)),
			Verdict: stuckqueue.VerdictUnmapped,
		})
	}
	c := stuckqueue.StuckCardForTest(items, true)
	if n := len(c.Plain()); n > card.MaxPlain {
		t.Errorf("plain length %d over %d", n, card.MaxPlain)
	}
	if n := c.Components(); n > card.MaxBuilt {
		t.Errorf("components %d over %d", n, card.MaxBuilt)
	}
	want := fmt.Sprintf("…and %d more\nStopped after clearing 20 this run.", 60-len(c.Items))
	if c.Footer != want {
		t.Errorf("footer = %q, want %q", c.Footer, want)
	}
	if c.Tone != card.Warn || c.Path != "/maintenance" || !strings.HasPrefix(c.Plain(), c.Title+"\n• Sonarr — ") {
		t.Errorf("card = %+v", c)
	}
}

func TestStuckCard_ComponentCap(t *testing.T) {
	t.Parallel()
	var items []stuckqueue.Item
	for range 60 {
		items = append(items, stuckqueue.Item{App: stuckqueue.AppRadarr, Verdict: stuckqueue.VerdictUnmapped})
	}
	c := stuckqueue.StuckCardForTest(items, false)
	if n := c.Components(); n > card.MaxBuilt || len(c.Items) == 0 {
		t.Errorf("components %d over %d with %d items", n, card.MaxBuilt, len(c.Items))
	}
	if c.Footer != fmt.Sprintf("…and %d more", 60-len(c.Items)) {
		t.Errorf("footer = %q", c.Footer)
	}
}

func TestStuckCard_ZeroItemsCap(t *testing.T) {
	t.Parallel()
	c := stuckqueue.StuckCardForTest(nil, true)
	if c.Plain() != "📦 Stuck-queue sweep: Stopped after clearing 20 this run." || c.Body != "" || c.Footer != "" {
		t.Errorf("card = %+v", c)
	}
}
