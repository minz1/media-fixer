package discord_test

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/machinetest"
)

func connSamples(s discord.ConnState) []discord.Conn {
	down := cardT0().Add(-time.Minute)
	switch s {
	case discord.ConnConnecting:
		return []discord.Conn{
			{State: s, Attempt: 1, Since: down, DownSince: down},
			{State: s, Attempt: 3, Since: down, DownSince: down, Err: "dial"},
		}
	case discord.ConnBackoff:
		return []discord.Conn{
			{State: s, Attempt: 1, Since: down, DownSince: down, RetryAt: cardT0(), Err: "dial"},
			{State: s, Attempt: 3, Since: down, DownSince: down, RetryAt: cardT0(), Err: "dial"},
		}
	case discord.ConnReconnecting:
		return []discord.Conn{{State: s, Since: down, DownSince: down}}
	case discord.ConnIdle, discord.ConnConnected:
		return []discord.Conn{{State: s, Since: down}}
	case discord.ConnClosed:
		return []discord.Conn{{State: s, Since: down}, {State: s, Since: down, Err: "dial"}}
	}
	return nil
}

func connEvents(k discord.ConnKind) []discord.ConnEvent {
	if k == discord.ConnOpenFailed {
		return []discord.ConnEvent{
			{Kind: k, At: cardT0(), Err: "dial", Wait: 5 * time.Second},
			{Kind: k, At: cardT0(), Wait: time.Minute},
		}
	}
	return []discord.ConnEvent{{Kind: k, At: cardT0()}}
}

func connMachine() machinetest.Machine[discord.ConnState, discord.ConnKind, discord.Conn, discord.ConnEvent] {
	return machinetest.Machine[discord.ConnState, discord.ConnKind, discord.Conn, discord.ConnEvent]{
		States:   discord.ConnStates(),
		Kinds:    discord.ConnKinds(),
		Initial:  discord.Conn{State: discord.ConnIdle},
		State:    func(c discord.Conn) discord.ConnState { return c.State },
		Key:      func(c discord.Conn) string { return string(c.State) },
		Samples:  connSamples,
		Events:   connEvents,
		Matches:  func(c discord.Conn, ev discord.ConnEvent) int { return discord.ConnMatches(c.State, ev.Kind) },
		Next:     discord.NextConn,
		Check:    discord.CheckConn,
		Terminal: func(s discord.ConnState) bool { return s == discord.ConnClosed },
	}
}

func TestConnMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, connMachine())
}

func TestConnMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, connMachine())
}

func TestConnMachine_Invariants(t *testing.T) {
	t.Parallel()
	m := connMachine()
	for _, s := range m.States {
		for _, c := range connSamples(s) {
			if err := discord.CheckConn(c); err != nil {
				t.Errorf("sample %+v fails its own check: %v", c, err)
			}
		}
	}
	machinetest.Invariants(t, m)
	broken := []discord.Conn{
		{State: discord.ConnBackoff, Attempt: 1, DownSince: cardT0()},
		{State: discord.ConnBackoff, Attempt: 1, DownSince: cardT0(), RetryAt: cardT0()},
		{State: discord.ConnConnecting, DownSince: cardT0()},
		{State: discord.ConnConnecting, Attempt: 1},
		{State: discord.ConnConnected, Attempt: 1},
		{State: discord.ConnConnected, DownSince: cardT0()},
		{State: discord.ConnClosed, RetryAt: cardT0()},
		{State: "bogus"},
	}
	for _, c := range broken {
		if discord.CheckConn(c) == nil {
			t.Errorf("check accepted %+v", c)
		}
	}
}

func connFuzzPath(kinds ...discord.ConnKind) []byte {
	var events []discord.ConnEvent
	for _, k := range discord.ConnKinds() {
		events = append(events, connEvents(k)...)
	}
	out := make([]byte, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, byte(slices.IndexFunc(events, func(e discord.ConnEvent) bool { return e.Kind == k })))
	}
	return out
}

func FuzzConnMachine(f *testing.F) {
	f.Add(connFuzzPath(discord.ConnStart, discord.ConnOpenOK, discord.ConnGatewayDown, discord.ConnGatewayUp))
	f.Add(connFuzzPath(discord.ConnStart, discord.ConnOpenFailed, discord.ConnRetryDue, discord.ConnOpenOK))
	f.Add(connFuzzPath(discord.ConnStart, discord.ConnOpenFailed, discord.ConnStop))
	for _, k := range discord.ConnKinds() {
		f.Add(connFuzzPath(k))
		f.Add(connFuzzPath(discord.ConnStart, k))
		f.Add(connFuzzPath(discord.ConnStart, discord.ConnOpenOK, k))
	}
	m := connMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func connStep(t *testing.T, c discord.Conn, ev discord.ConnEvent) discord.Conn {
	t.Helper()
	next, err := discord.NextConn(c, ev)
	if err != nil {
		t.Fatalf("%+v x %+v: %v", c, ev, err)
	}
	if cerr := discord.CheckConn(next); cerr != nil {
		t.Fatalf("%+v x %+v = %+v: %v", c, ev, next, cerr)
	}
	return next
}

func TestConnMachine_BackoffExitsClearRetryAt(t *testing.T) {
	t.Parallel()
	c := connStep(t, discord.Conn{State: discord.ConnIdle}, discord.ConnEvent{Kind: discord.ConnStart, At: cardT0()})
	c = connStep(t, c, discord.ConnEvent{Kind: discord.ConnOpenFailed, At: cardT0(), Err: "dial", Wait: time.Minute})
	if c.State != discord.ConnBackoff || !c.RetryAt.Equal(cardT0().Add(time.Minute)) || c.Err != "dial" {
		t.Fatalf("after OpenFailed: %+v", c)
	}
	retried := connStep(t, c, discord.ConnEvent{Kind: discord.ConnRetryDue, At: cardT0().Add(time.Minute)})
	if retried.State != discord.ConnConnecting || retried.Attempt != 2 || !retried.RetryAt.IsZero() {
		t.Errorf("RetryDue = %+v, want connecting attempt 2 with no RetryAt", retried)
	}
	stopped := connStep(t, c, discord.ConnEvent{Kind: discord.ConnStop, At: cardT0().Add(time.Second)})
	if stopped.State != discord.ConnClosed || !stopped.RetryAt.IsZero() || stopped.Attempt != 0 ||
		!stopped.DownSince.IsZero() || stopped.Err != "dial" {
		t.Errorf("Stop from backoff = %+v, want closed keeping only Err", stopped)
	}
}

func TestConnMachine_DownSinceKeptAcrossRetries(t *testing.T) {
	t.Parallel()
	c := connStep(t, discord.Conn{State: discord.ConnIdle}, discord.ConnEvent{Kind: discord.ConnStart, At: cardT0()})
	at := cardT0()
	for i := range 3 {
		at = at.Add(time.Second)
		c = connStep(
			t,
			c,
			discord.ConnEvent{Kind: discord.ConnOpenFailed, At: at, Err: strconv.Itoa(i), Wait: time.Second},
		)
		at = at.Add(time.Second)
		c = connStep(t, c, discord.ConnEvent{Kind: discord.ConnRetryDue, At: at})
		if !c.DownSince.Equal(cardT0()) || c.Attempt != i+2 {
			t.Fatalf("retry %d: %+v, want DownSince %v", i, c, cardT0())
		}
	}
	c = connStep(t, c, discord.ConnEvent{Kind: discord.ConnOpenOK, At: at})
	if c.State != discord.ConnConnected || !c.DownSince.IsZero() || c.Err != "" || c.Attempt != 0 ||
		!c.Since.Equal(at) {
		t.Fatalf("connected = %+v", c)
	}
	drop := at.Add(time.Hour)
	c = connStep(t, c, discord.ConnEvent{Kind: discord.ConnGatewayDown, At: drop})
	c = connStep(t, c, discord.ConnEvent{Kind: discord.ConnGatewayDown, At: drop.Add(time.Minute)})
	if c.State != discord.ConnReconnecting || !c.DownSince.Equal(drop) {
		t.Errorf("gateway drop = %+v, want reconnecting since %v", c, drop)
	}
}

func TestMachineDiagram_Conn(t *testing.T) {
	t.Parallel()
	machinetest.GoldenMermaid(t, connMachine(), "testdata/conn.md")
}
