package stuckqueue_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
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

func buttonVerbs(t *testing.T, bs []card.Button) []string {
	t.Helper()
	var out []string
	for _, b := range bs {
		a, err := card.Decode(b.ID)
		if err != nil {
			t.Fatalf("button %q: %v", b.ID, err)
		}
		out = append(out, a.Verb)
	}
	return out
}

func actionVerbs(as []stuckqueue.Action) []string {
	var out []string
	for _, a := range as {
		out = append(out, string(a))
	}
	return out
}

func TestStuckCard_ItemButtonsFollowOffered(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.loadFixture(t, "../client/testdata/sonarr-queue-2026-10-29.json")
	set := liveSettings()
	set.DryRun = true
	res := e.agedRun(set)
	e.mu.Lock()
	cards := slices.Clone(e.cards)
	e.mu.Unlock()
	if len(cards) != 1 || len(cards[0].Items) == 0 {
		t.Fatalf("cards = %+v", cards)
	}
	items := app(res, stuckqueue.AppSonarr).Items
	offers := map[string]int{}
	for _, ci := range cards[0].Items {
		i := slices.IndexFunc(items, func(it stuckqueue.Item) bool {
			return ci.Subject == card.StuckSubject("s", it.DownloadID)
		})
		if i < 0 {
			t.Fatalf("card item %q has no stuck download", ci.Subject)
		}
		it := items[i]
		if got, want := buttonVerbs(t, ci.Buttons), actionVerbs(it.Actions); !slices.Equal(got, want) {
			t.Errorf("%s buttons = %v, want %v", it.Title, got, want)
		}
		offers[strings.Join(actionVerbs(it.Actions), ",")]++
		for _, b := range ci.Buttons {
			a, _ := card.Decode(b.ID)
			if a.Kind != "sq" || !slices.Equal(a.Args, []string{"s", it.DownloadID, it.TargetsFP()}) {
				t.Errorf("button %q args = %v", b.ID, a.Args)
			}
		}
	}
	if len(offers) < 2 {
		t.Errorf("fixture cards offer only %v; want items with different offered actions", offers)
	}
	if c := cards[0]; c.Components() > card.MaxBuilt {
		t.Errorf("components %d over %d", c.Components(), card.MaxBuilt)
	}
}

func TestStuckCard_TwentyItemsWithinComponents(t *testing.T) {
	t.Parallel()
	all := []stuckqueue.Action{stuckqueue.ActionImport, stuckqueue.ActionRegrab, stuckqueue.ActionRemove}
	var items []stuckqueue.Item
	for i := range 20 {
		items = append(items, stuckqueue.Item{
			App: stuckqueue.AppSonarr, DownloadID: fmt.Sprintf("SABnzbd_nzo_%040d", i), Title: "NOVA",
			Verdict: stuckqueue.VerdictGap,
			Actions: all,
		})
	}
	c := stuckqueue.StuckCardForTest(items, false)
	if n := c.Components(); n > card.MaxBuilt {
		t.Fatalf("components %d over %d", n, card.MaxBuilt)
	}
	if len(c.Items) == 0 || len(c.Items) == 20 {
		t.Fatalf("items = %d, want some kept and the rest counted", len(c.Items))
	}
	if c.Footer != fmt.Sprintf("…and %d more", 20-len(c.Items)) {
		t.Errorf("footer = %q", c.Footer)
	}
	for _, it := range c.Items {
		if len(it.Buttons) != len(all) {
			t.Errorf("%s buttons = %d, want %d", it.Subject, len(it.Buttons), len(all))
		}
	}
}

func stuckItem(app stuckqueue.App, dl string, actions ...stuckqueue.Action) stuckqueue.Item {
	return stuckqueue.Item{
		App: app, DownloadID: dl, Title: dl,
		Records: []client.QueueEntry{{DownloadID: dl, SeasonNumber: 1, EpisodeNumber: 2}},
		Verdict: stuckqueue.VerdictGap,
		Actions: actions,
	}
}

func lastWith(apps ...stuckqueue.AppResult) stuckqueue.Result {
	return stuckqueue.Result{Apps: apps}
}

func render(t *testing.T, s *stuckqueue.Scheduler, subject string) (card.Update, bool) {
	t.Helper()
	up, ok, err := s.CardRenderer()(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return up, ok
}

func TestRenderer_StillStuck(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	it := stuckItem(stuckqueue.AppSonarr, "ABC", stuckqueue.ActionRegrab, stuckqueue.ActionRemove)
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{
		App: stuckqueue.AppSonarr, Configured: true, Items: []stuckqueue.Item{it},
	}), false)
	up, ok := render(t, s, card.StuckSubject("s", "abc"))
	if !ok || up.Status != "" {
		t.Fatalf("update = %+v, %v", up, ok)
	}
	if got := buttonVerbs(t, up.Buttons); !slices.Equal(got, []string{"regrab", "remove"}) {
		t.Fatalf("verbs = %v", got)
	}
	a, _ := card.Decode(up.Buttons[0].ID)
	if !slices.Equal(a.Args, []string{"s", "ABC", it.TargetsFP()}) {
		t.Fatalf("args = %v", a.Args)
	}
}

func TestRenderer_Acted(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove); err != nil {
		t.Fatal(err)
	}
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{
		App: stuckqueue.AppSonarr, Configured: true,
		Items: []stuckqueue.Item{stuckItem(stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove)},
	}), false)
	up, ok := render(t, s, card.StuckSubject("s", "D"))
	want := fmt.Sprintf("✅ Removed · <t:%d:t>", e.clock.Now().Unix())
	if !ok || up.Status != want || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v, %v; want status %q and no buttons", up, ok, want)
	}
}

func TestRenderer_Gone(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{App: stuckqueue.AppSonarr, Configured: true}), false)
	up, ok := render(t, s, card.StuckSubject("s", "abc"))
	if !ok || up.Status != "left the queue" || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v, %v", up, ok)
	}
}

func TestRenderer_WaitingKeepsRemove(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	w := stuckItem(stuckqueue.AppSonarr, "ABC", stuckqueue.ActionRemove)
	w.Due = time.Date(2026, 10, 30, 5, 7, 0, 0, time.UTC)
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{
		App: stuckqueue.AppSonarr, Configured: true, Waiting: []stuckqueue.Item{w},
	}), false)
	up, ok := render(t, s, card.StuckSubject("s", "abc"))
	if !ok || !strings.Contains(up.Status, fmt.Sprintf("<t:%d:t>", w.Due.Unix())) {
		t.Fatalf("update = %+v, %v; want the due time", up, ok)
	}
	if got := buttonVerbs(t, up.Buttons); !slices.Equal(got, []string{"remove"}) {
		t.Fatalf("verbs = %v, want only remove", got)
	}
}

func TestRenderer_UnreachableAppUnknown(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	stuckqueue.SetLastForTest(s, lastWith(
		stuckqueue.AppResult{App: stuckqueue.AppSonarr, Configured: true, Unreachable: true},
		stuckqueue.AppResult{
			App: stuckqueue.AppRadarr, Configured: true,
			Items: []stuckqueue.Item{stuckItem(stuckqueue.AppRadarr, "M1", stuckqueue.ActionRemove)},
		},
	), false)
	if up, ok := render(t, s, card.StuckSubject("s", "abc")); ok {
		t.Fatalf("unreachable sonarr rendered %+v", up)
	}
	if _, ok := render(t, s, card.StuckSubject("r", "m1")); ok {
		return
	}
	t.Fatal("radarr item did not render")
}

func TestRenderer_UnconfiguredAppUnknown(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{App: stuckqueue.AppRadarr}), false)
	if up, ok := render(t, s, card.StuckSubject("r", "m1")); ok {
		t.Fatalf("unconfigured radarr rendered %+v", up)
	}
}

func TestRenderer_BeforeFirstRunUnknown(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	if up, ok := render(t, s, card.StuckSubject("s", "abc")); ok {
		t.Fatalf("rendered %+v before any run", up)
	}
}

func TestRenderer_RunActiveKeepsLast(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	it := stuckItem(stuckqueue.AppSonarr, "ABC", stuckqueue.ActionRegrab, stuckqueue.ActionRemove)
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{
		App: stuckqueue.AppSonarr, Configured: true, Items: []stuckqueue.Item{it},
	}), true)
	up, ok := render(t, s, card.StuckSubject("s", "abc"))
	if got := buttonVerbs(t, up.Buttons); !ok || !slices.Equal(got, []string{"regrab", "remove"}) {
		t.Fatalf("update during a run = %+v, %v; want the last result's buttons", up, ok)
	}
}

func TestRenderer_StuckAgainAfterActClearsOutcome(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove); err != nil {
		t.Fatal(err)
	}
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	e.tickAt(s, time.Minute)
	up, ok := render(t, s, card.StuckSubject("s", "D"))
	if !ok || strings.HasPrefix(up.Status, "✅") || len(up.Buttons) == 0 {
		t.Fatalf("update = %+v, %v; want the still-stuck buttons, not the old outcome", up, ok)
	}
}

func TestRenderer_BadSubjectUnknown(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{App: stuckqueue.AppSonarr, Configured: true}), false)
	for _, subj := range []string{"sq:x:abc", "sq:s", "inc:1"} {
		if up, ok := render(t, s, subj); ok {
			t.Errorf("%q rendered %+v", subj, up)
		}
	}
}

func TestAct_RecordsOutcomeThenRefreshesSubject(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	var mu sync.Mutex
	var calls []string
	var statuses []string
	s.Refresh = func(subject string) {
		up, _, _ := s.CardRenderer()(context.Background(), subject)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, subject)
		statuses = append(statuses, up.Status)
	}
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{card.StuckSubject("s", "D")}) {
		t.Fatalf("refresh calls = %v", calls)
	}
	if !strings.HasPrefix(statuses[0], "✅ Removed · ") {
		t.Fatalf("status at refresh = %q, want the outcome", statuses[0])
	}
}

func TestAct_RefusedRecordsNoOutcome(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	called := false
	s.Refresh = func(string) { called = true }
	if _, err := act(e, s, stuckqueue.AppSonarr, "GONE", stuckqueue.ActionRemove); err == nil {
		t.Fatal("acted on a download that is not queued")
	}
	stuckqueue.SetLastForTest(s, lastWith(stuckqueue.AppResult{App: stuckqueue.AppSonarr, Configured: true}), false)
	if up, _ := render(t, s, card.StuckSubject("s", "gone")); up.Status != "left the queue" || called {
		t.Fatalf("update = %+v, refreshed %v", up, called)
	}
}

func handle(t *testing.T, s *stuckqueue.Scheduler, verb string, args ...string) card.Result {
	t.Helper()
	res, err := s.CardHandler()(context.Background(), card.Action{Kind: "sq", Verb: verb, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHandler_ActsWithFingerprint(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if _, err := e.db.UpsertStuckSeen(context.Background(), "sonarr", "D", "D", "", e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	fp := card.Fingerprint([]byte("S01E01"))
	if res := handle(t, s, "remove", "s", "D", fp); res.Line != "Removed." {
		t.Fatalf("line = %q", res.Line)
	}
	if n := len(deletes(e.sonarr.writesCopy())); n != 1 {
		t.Fatalf("deletes = %d", n)
	}
}

func TestHandler_RefusalLines(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	cases := map[string][]string{
		"Refused: this download now maps to S01E01; check the card again": {"remove", "s", "D", "000000000000"},
		"Refused: this download is no longer in the Sonarr queue":         {"remove", "s", "GONE", "x"},
		"Refused: Radarr is not configured":                               {"remove", "r", "D", "x"},
	}
	e.runner.Radarr = nil
	for want, a := range cases {
		if res := handle(t, s, a[0], a[1:]...); res.Line != want {
			t.Errorf("%v: line = %q, want %q", a, res.Line, want)
		}
	}
	if n := len(e.sonarr.writesCopy()); n != 0 {
		t.Fatalf("writes = %d, want none", n)
	}
}

func TestHandler_BadArgs(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	for _, args := range [][]string{{"s", "D"}, {"x", "D", "fp"}} {
		_, err := s.CardHandler()(context.Background(), card.Action{Kind: "sq", Verb: "remove", Args: args})
		if err == nil {
			t.Errorf("args %v: no error", args)
		}
	}
}
