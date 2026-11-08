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
	"github.com/minz1/mediafixer/internal/operation"
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
	if c.Plain() != "Stuck-queue sweep: Stopped after clearing 20 this run." || c.Body != "" || c.Footer != "" {
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

func render(t *testing.T, s *stuckqueue.Scheduler, subject string) (card.Update, bool) {
	t.Helper()
	r, err := s.CardRenderer()(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return r.Update, r.Outcome == card.Render
}

func notifiedRow(e *runEnv, offer stuckqueue.Offer, fp string) stuckqueue.Snapshot {
	return stuckqueue.Snapshot{
		Key: sonarrKey("ABC"), State: stuckqueue.StateNotified, FirstSeen: e.clock.Now().Add(-8 * time.Hour),
		NotifiedAt: e.clock.Now().Add(-time.Hour), TargetsFP: fp, Offered: offer,
	}
}

func TestRenderer_StillStuck(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	seedSnap(t, e, notifiedRow(e, stuckqueue.OfferRegrabRemove, "fp1"))
	up, ok := render(t, s, card.StuckSubject("s", "abc"))
	if !ok || up.Status != "" {
		t.Fatalf("update = %+v, %v", up, ok)
	}
	if got := buttonVerbs(t, up.Buttons); !slices.Equal(got, []string{"regrab", "remove"}) {
		t.Fatalf("verbs = %v", got)
	}
	a, _ := card.Decode(up.Buttons[0].ID)
	if !slices.Equal(a.Args, []string{"s", "ABC", "fp1"}) {
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
	up, ok := render(t, s, card.StuckSubject("s", "D"))
	want := fmt.Sprintf("✅ Removed · <t:%d:t>", e.clock.Now().Unix())
	if !ok || up.Status != want || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v, %v; want status %q and no buttons", up, ok, want)
	}
}

func TestRenderer_Gone(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	up, ok := render(t, s, card.StuckSubject("s", "abc"))
	if !ok || up.Status != "left the queue" || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v, %v", up, ok)
	}
}

func TestRenderer_WaitingKeepsRemove(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	w := waitingAt(e, time.Hour)
	w.Key = sonarrKey("ABC")
	seedSnap(t, e, w)
	due := w.FirstSeen.Add(6 * time.Hour)
	up, ok := render(t, s, card.StuckSubject("s", "abc"))
	if want := fmt.Sprintf("⏳ until <t:%d:t>", due.Unix()); !ok || up.Status != want {
		t.Fatalf("update = %+v, %v; want status %q", up, ok, want)
	}
	if got := buttonVerbs(t, up.Buttons); !slices.Equal(got, []string{"remove"}) {
		t.Fatalf("verbs = %v, want only remove", got)
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
	e.tickAt(s, 11*time.Minute)
	up, ok := render(t, s, card.StuckSubject("s", "D"))
	if !ok || strings.HasPrefix(up.Status, "✅") || len(up.Buttons) == 0 {
		t.Fatalf("update = %+v, %v; want the still-stuck buttons, not the old outcome", up, ok)
	}
}

func TestRenderer_BadSubjectUnknown(t *testing.T) {
	t.Parallel()
	_, s := newSched(t)
	for _, subj := range []string{"sq:x:abc", "sq:s", "inc:1"} {
		if up, ok := render(t, s, subj); ok {
			t.Errorf("%q rendered %+v", subj, up)
		}
	}
}

func renderedFor(t *testing.T, s *stuckqueue.Scheduler) card.Update {
	t.Helper()
	r, err := s.CardRenderer()(context.Background(), card.StuckSubject("s", "D"))
	if err != nil || r.Outcome != card.Render {
		t.Fatalf("rendering = %+v, err = %v", r, err)
	}
	return r.Update
}

func TestQ2_3_CardShowsImportingWithoutButtons(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	row := inflightRow(e, 5)
	seedSnap(t, e, row)
	up := renderedFor(t, s)
	want := fmt.Sprintf("⏳ Importing since <t:%d:t>", row.Import.StartedAt.Unix())
	if up.Status != want || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v; want status %q and no buttons", up, want)
	}
}

func TestQ2_10_ActedOutcomeSurvivesRestart(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove); err != nil {
		t.Fatal(err)
	}
	s2 := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s2.Wait)
	up := renderedFor(t, s2)
	want := fmt.Sprintf("✅ Removed · <t:%d:t>", e.clock.Now().Unix())
	if up.Status != want || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v; want status %q and no buttons", up, want)
	}
}

func TestStartup_TrackedCardRendersBeforeFirstRun(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	row := notifiedRow(e, stuckqueue.OfferAll, "fp9")
	row.Key = sonarrKey("D")
	seedSnap(t, e, row)
	up := renderedFor(t, s)
	if got := buttonVerbs(t, up.Buttons); !slices.Equal(got, []string{"import", "regrab", "remove"}) {
		t.Fatalf("verbs = %v", got)
	}
	for _, b := range up.Buttons {
		if a, _ := card.Decode(b.ID); !slices.Equal(a.Args, []string{"s", "D", "fp9"}) {
			t.Fatalf("args = %v, want the stored fingerprint", a.Args)
		}
	}
}

func TestStartup_MigratedRowDropsOldButtons(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	row := notifiedRow(e, stuckqueue.OfferNone, "")
	row.Key = sonarrKey("D")
	seedSnap(t, e, row)
	up := renderedFor(t, s)
	if len(up.Buttons) != 0 || up.Status != "" {
		t.Fatalf("update = %+v, want no buttons and no status until the first run", up)
	}
}

func TestCard_ImportNotesWithButtons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		result stuckqueue.ActResult
		files  stuckqueue.Files
		want   string
	}{
		{stuckqueue.ResultImportFailed, stuckqueue.Files{N: 2, M: 2}, "⚠️ Import failed"},
		{stuckqueue.ResultImportTimeout, stuckqueue.Files{N: 2, M: 2}, "⚠️ Import still running"},
		{stuckqueue.ResultImportUnconfirmed, stuckqueue.Files{N: 2, M: 2}, "⚠️ Import not confirmed"},
		{stuckqueue.ResultPartial, stuckqueue.Files{N: 1, M: 2}, "⚠️ Imported 1 of 2 files"},
	} {
		t.Run(string(tc.result), func(t *testing.T) {
			t.Parallel()
			e, s := newSched(t)
			row := notifiedRow(e, stuckqueue.OfferAll, "fp")
			row.Key = sonarrKey("D")
			row.Acted = &stuckqueue.Acted{
				Action: stuckqueue.ActionImport, At: e.clock.Now().Add(-time.Minute),
				Result: tc.result, Files: tc.files,
			}
			seedSnap(t, e, row)
			up := renderedFor(t, s)
			if up.Status != tc.want {
				t.Errorf("status = %q, want %q", up.Status, tc.want)
			}
			if got := buttonVerbs(t, up.Buttons); !slices.Equal(got, []string{"import", "regrab", "remove"}) {
				t.Errorf("verbs = %v", got)
			}
		})
	}
}

func TestCard_DueAndNotifiedHaveNoStatusLine(t *testing.T) {
	t.Parallel()
	for _, st := range []stuckqueue.State{stuckqueue.StateDue, stuckqueue.StateNotified} {
		t.Run(string(st), func(t *testing.T) {
			t.Parallel()
			e, s := newSched(t)
			row := notifiedRow(e, stuckqueue.OfferRegrabRemove, "fp")
			row.Key, row.State = sonarrKey("D"), st
			if st == stuckqueue.StateDue {
				row.NotifiedAt = time.Time{}
			}
			seedSnap(t, e, row)
			up := renderedFor(t, s)
			if up.Status != "" || len(up.Buttons) != 2 {
				t.Fatalf("update = %+v, want buttons and no status", up)
			}
		})
	}
}

func TestCard_UnfinishedRegrabShowsLineAndResume(t *testing.T) {
	t.Parallel()
	e, _, op := failSearch(t)
	s2 := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s2.Wait)
	up := renderedFor(t, s2)
	if up.Status != "⚠️ "+operation.Line(op) {
		t.Fatalf("status = %q", up.Status)
	}
	a := resumeButton(t, up)
	want := []string{"s", "D", "o" + itoa(op.ID) + "." + itoa(op.Version)}
	if !slices.Equal(a.Args, want) {
		t.Fatalf("args = %v, want %v", a.Args, want)
	}
}

func TestCard_UnfinishedRegrabOpDoneShowsRegrabbed(t *testing.T) {
	t.Parallel()
	e, s, op := failSearch(t)
	e.rawExec(t, fmt.Sprintf("UPDATE operations SET status = 'done', cause = '' WHERE id = %d", op.ID))
	up := renderedFor(t, s)
	if !strings.HasPrefix(up.Status, "✅ Re-grabbed") || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v", up)
	}
}

func TestCard_UnfinishedRegrabOpMissingHasNoButton(t *testing.T) {
	t.Parallel()
	e, s, _ := failSearch(t)
	e.rawExec(t, "DELETE FROM operations")
	up := renderedFor(t, s)
	if up.Status != "⚠️ Re-grab didn't finish" || len(up.Buttons) != 0 {
		t.Fatalf("update = %+v", up)
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
		r, _ := s.CardRenderer()(context.Background(), subject)
		up := r.Update
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
	_, s := newSched(t)
	called := false
	s.Refresh = func(string) { called = true }
	if _, err := s.Act(context.Background(), stuckqueue.AppSonarr, "GONE", stuckqueue.ActionRemove, ""); err == nil {
		t.Fatal("acted on a download that is not queued")
	}
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
	if err := seedSeen(e, stuckqueue.AppSonarr, "D", e.clock.Now().Add(-7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	fp := card.Fingerprint([]byte("e1"))
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
		"Refused: this changed, look again":                       {"remove", "s", "D", "000000000000"},
		"Refused: this download is no longer in the Sonarr queue": {"remove", "s", "GONE", "x"},
		"Refused: Radarr is not configured":                       {"remove", "r", "D", "x"},
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

func TestStuckRenderer_Outcomes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		subject string
		want    card.Outcome
	}{
		{"malformed subject", "sq:s", card.Gone},
		{"wrong kind", "inc:1", card.Gone},
		{"unknown app code", "sq:x:abc", card.Gone},
		{"no row", card.StuckSubject("s", "abc"), card.Render},
		{"stored row, any case", card.StuckSubject("s", "d"), card.Render},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, s := newSched(t)
			row := notifiedRow(e, stuckqueue.OfferRemove, "fp")
			row.Key = sonarrKey("D")
			seedSnap(t, e, row)
			r, err := s.CardRenderer()(context.Background(), tc.subject)
			if err != nil || r.Outcome != tc.want {
				t.Errorf("outcome = %v err = %v, want %v", r.Outcome, err, tc.want)
			}
		})
	}
	t.Run("db read error", func(t *testing.T) {
		t.Parallel()
		e, s := newSched(t)
		e.rawExec(t, "DROP TABLE stuck_downloads")
		r, err := s.CardRenderer()(context.Background(), card.StuckSubject("s", "abc"))
		if err == nil || r.Outcome == card.Render {
			t.Errorf("rendering = %+v err = %v, want an error", r, err)
		}
	})
	t.Run("owner outcome recorded", func(t *testing.T) {
		t.Parallel()
		e, s := newSched(t)
		e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
		if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove); err != nil {
			t.Fatal(err)
		}
		r, err := s.CardRenderer()(context.Background(), card.StuckSubject("s", "D"))
		if err != nil || r.Outcome != card.Render || !strings.HasPrefix(r.Update.Status, "✅") {
			t.Errorf("rendering = %+v err = %v, want Render with the outcome line", r, err)
		}
	})
}
