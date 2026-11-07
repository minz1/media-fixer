package discord_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/discord"
)

func TestA10_4_PressArrivingOnConnectIsHandled(t *testing.T) {
	t.Parallel()
	e := newClickEnv(t, "owner")
	ran := make(chan struct{}, 1)
	e.handlers["inc"] = func(context.Context, card.Action) (card.Result, error) {
		ran <- struct{}{}
		return card.Result{Line: "ran"}, nil
	}
	e.track("inc:1", "m9", incCard("1"), time.Now())
	var connects atomic.Int32
	discord.SetConnectForTest(e.bot, func() error {
		connects.Add(1)
		e.press(dmClick("owner", "m9", btn("inc", "resolve", "1", "s").ID))
		return nil
	})
	e.start(map[string]card.Renderer{"inc": newRenderer(true).render})
	e.bot.Start(t.Context())
	select {
	case <-ran:
	case <-time.After(waitLimit):
		for _, r := range e.click.respondList() {
			if r.Data != nil {
				t.Logf("reply: %q", r.Data.Content)
			}
		}
		t.Fatalf("press on connect was not handled; connects = %d", connects.Load())
	}
}
