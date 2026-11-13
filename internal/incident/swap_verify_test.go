package incident_test

import (
	"context"
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

type swapVerifyAgent struct {
	*subtitleAgent

	landed bool
	newID  string
}

func (a *swapVerifyAgent) SwapItem(context.Context, lifecycle.SwapTarget) (string, bool, string) {
	return a.newID, a.landed, ""
}

func swapVerifying(t *testing.T, env *lcEnv) lifecycle.Snapshot {
	t.Helper()
	s := env.verifying(t, "Show", "subtitles")
	return lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) {
		n.ItemID = "jf-old"
		n.Verify.Spec = lifecycle.VerifySpec{
			Kind: lifecycle.VerifySubtitles, ItemID: "jf-old", Language: "eng", SubtitleItemID: "jf-old",
			Swap: &lifecycle.SwapTarget{SeriesItemID: "jf-series", Season: 1, Episode: 6, EpisodeID: 66, OldFileID: 41},
		}
	})
}

func TestD8_SwapVerifyPasses(t *testing.T) {
	t.Parallel()
	ag := &swapVerifyAgent{
		subtitleAgent: &subtitleAgent{observingAgent: newObservingAgent()}, landed: true, newID: "jf-new",
	}
	env := newLifecycleEnv(t, ag)
	s := swapVerifying(t, env)
	env.svc.StartRecovery()
	env.waitFixed(t, s.ID)
	if ag.item.Load() != "jf-new" || ag.subs.Load() != 1 {
		t.Fatalf("VerifySubtitle ran %d times on %v, want once on jf-new", ag.subs.Load(), ag.item.Load())
	}
	if got := env.snap(t, s.ID).ItemID; got != "jf-new" {
		t.Fatalf("snapshot ItemID = %q", got)
	}
}

func TestD8_JellyfinItemIDWritten(t *testing.T) {
	t.Parallel()
	ag := &swapVerifyAgent{
		subtitleAgent: &subtitleAgent{observingAgent: newObservingAgent()}, landed: true, newID: "jf-new",
	}
	env := newLifecycleEnv(t, ag)
	s := swapVerifying(t, env)
	other := env.verifying(t, "Other", "cant_play")
	keep := env.verifying(t, "Keep", "cant_play")
	lifecycletest.Force(t, env.db, keep.ID, func(n *lifecycle.Snapshot) { n.ItemID = "keep-id" })
	env.svc.StartRecovery()
	env.waitFixed(t, s.ID)

	for id, want := range map[string]string{s.ID: "jf-new", keep.ID: "keep-id"} {
		inc, err := env.db.GetIncident(t.Context(), id)
		if err != nil || inc.JellyfinItemID != want {
			t.Fatalf("incident %s: jellyfin_item_id %q, err %v, want %q", id, inc.JellyfinItemID, err, want)
		}
	}
	inc, err := env.db.GetIncident(t.Context(), other.ID)
	if err != nil || inc.JellyfinItemID != "" {
		t.Fatalf("a non-swap incident gained jellyfin_item_id %q, err %v", inc.JellyfinItemID, err)
	}
}
