package server_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/server"
)

func wireDiscord(srv *server.Server, c discord.Conn) {
	srv.SetDiscord(func() discord.Conn { return c })
}

func TestSelftest_ShowsDiscordConnected(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	wireDiscord(srv, discord.Conn{State: discord.ConnConnected, Since: time.Now()})
	body := getBody(srv, "/media/selftest")
	if !strings.Contains(body, "Discord connected") || !strings.Contains(body, "tone-green") {
		t.Errorf("missing connected badge")
	}
}

func TestSelftest_ShowsDiscordBackoffWithInfo(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	wireDiscord(srv, discord.Conn{
		State: discord.ConnBackoff, DownSince: time.Now(), RetryAt: time.Date(2026, 11, 8, 12, 34, 0, 0, time.UTC),
		Err: "dial tcp: boom",
	})
	body := getBody(srv, "/media/selftest")
	for _, want := range []string{"Discord offline", "tone-red", "Retrying at ", "12:34", "dial tcp: boom"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestPageTop_DiscordBadgeOnlyAfterTwoMinutesDown(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	wireDiscord(srv, discord.Conn{State: discord.ConnBackoff, DownSince: time.Now().Add(-time.Minute)})
	if strings.Contains(getBody(srv, "/media/"), "Discord") {
		t.Errorf("badge shown after 1 minute down")
	}
	wireDiscord(srv, discord.Conn{State: discord.ConnBackoff, DownSince: time.Now().Add(-3 * time.Minute)})
	if !strings.Contains(getBody(srv, "/media/"), "Discord offline") {
		t.Errorf("badge missing after 3 minutes down")
	}
	wireDiscord(srv, discord.Conn{State: discord.ConnReconnecting, DownSince: time.Now().Add(-3 * time.Minute)})
	if !strings.Contains(getBody(srv, "/media/"), "Discord connecting") {
		t.Errorf("connecting badge missing after 3 minutes down")
	}
}

func TestPageTop_NoBadgeWhenConnectedIdleOrClosed(t *testing.T) {
	t.Parallel()
	for _, st := range []discord.ConnState{discord.ConnConnected, discord.ConnIdle, discord.ConnClosed} {
		srv, _ := newTestServer(t)
		wireDiscord(srv, discord.Conn{State: st, DownSince: time.Now().Add(-time.Hour)})
		if strings.Contains(getBody(srv, "/media/"), "Discord") {
			t.Errorf("%s: badge shown in header", st)
		}
	}
}

func TestPageTop_NoDiscordWiredShowsNothing(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	for _, p := range []string{"/media/", "/media/selftest"} {
		if strings.Contains(getBody(srv, p), "Discord") {
			t.Errorf("%s mentions Discord with nothing wired", p)
		}
	}
}

func TestServer_SetDiscordAfterNewIsSeenByTemplates(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 20 {
				getBody(srv, "/media/selftest")
			}
		})
	}
	wireDiscord(srv, discord.Conn{State: discord.ConnConnected})
	wg.Wait()
	if !strings.Contains(getBody(srv, "/media/selftest"), "Discord connected") {
		t.Errorf("late SetDiscord not seen")
	}
}
