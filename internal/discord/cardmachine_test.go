package discord_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/machinetest"
)

func cardT0() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }

const (
	sampleRate  = 30 * time.Second
	sampleRetry = time.Minute
)

func cardSamples(p discord.CardPhase) []discord.CardSnap {
	switch p {
	case discord.CardFailing:
		var out []discord.CardSnap
		for _, f := range []int{1, 3} {
			out = append(out,
				discord.CardSnap{Phase: p, Failures: f, NextRetry: cardT0().Add(sampleRetry)},
				discord.CardSnap{
					Phase: p, Failures: f, NextRetry: cardT0().Add(sampleRetry), RateUntil: cardT0().Add(sampleRate),
				},
			)
		}
		return out
	case discord.CardDirty:
		return []discord.CardSnap{{Phase: p}, {Phase: p, Failures: 1}, {Phase: p, Failures: 3}}
	case discord.CardCurrent, discord.CardLive, discord.CardGone:
		return []discord.CardSnap{{Phase: p}}
	}
	return nil
}

func cardAts() []time.Time {
	return []time.Time{cardT0(), cardT0().Add(sampleRate + 15*time.Second), cardT0().Add(2 * sampleRetry)}
}

func cardEvents(k discord.CardKind) []discord.CardEvent {
	var out []discord.CardEvent
	for _, at := range cardAts() {
		switch k {
		case discord.CardShown:
			out = append(out, discord.CardEvent{Kind: k, At: at}, discord.CardEvent{Kind: k, At: at, Live: true})
		case discord.CardFailed:
			out = append(out,
				discord.CardEvent{Kind: k, At: at}, discord.CardEvent{Kind: k, At: at, RetryAfter: time.Hour})
		case discord.CardMark, discord.CardPress, discord.CardRetryDue, discord.CardHeld,
			discord.CardMissing, discord.CardUndecodable, discord.CardAllGone, discord.CardPruned:
			out = append(out, discord.CardEvent{Kind: k, At: at})
		}
	}
	return out
}

func cardKey(s discord.CardSnap) string { return fmt.Sprintf("%s/%v", s.Phase, s.Failures > 0) }

func cardBackoffObligation(before discord.CardSnap, ev discord.CardEvent) error {
	if before.Phase != discord.CardDirty || ev.Kind != discord.CardFailed {
		return nil
	}
	after, err := discord.NextCard(before, ev)
	if err != nil {
		return fmt.Errorf("dirty rejected Failed: %w", err)
	}
	if after.Failures != before.Failures+1 {
		return fmt.Errorf("failures %d -> %d", before.Failures, after.Failures)
	}
	want := max(discord.CardBackoff(after.Failures, discord.CardRetry{}), ev.RetryAfter)
	if got := after.NextRetry.Sub(ev.At); got != want {
		return fmt.Errorf("next retry in %v, want %v", got, want)
	}
	return nil
}

func cardMachine() machinetest.Machine[discord.CardPhase, discord.CardKind, discord.CardSnap, discord.CardEvent] {
	return machinetest.Machine[discord.CardPhase, discord.CardKind, discord.CardSnap, discord.CardEvent]{
		States:   discord.CardPhases(),
		Kinds:    discord.CardKinds(),
		Initial:  discord.CardSnap{Phase: discord.CardCurrent},
		State:    func(s discord.CardSnap) discord.CardPhase { return s.Phase },
		Key:      cardKey,
		Samples:  cardSamples,
		Events:   cardEvents,
		Matches:  func(s discord.CardSnap, ev discord.CardEvent) int { return discord.CardMatches(s.Phase, ev.Kind) },
		Next:     discord.NextCard,
		Check:    discord.CheckCard,
		Terminal: func(p discord.CardPhase) bool { return p == discord.CardGone },
		Extra:    cardBackoffObligation,
	}
}

func TestCardMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, cardMachine())
}

func TestCardMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, cardMachine())
}

func TestCardMachine_Invariants(t *testing.T) {
	t.Parallel()
	m := cardMachine()
	for _, p := range m.States {
		for _, s := range cardSamples(p) {
			if err := discord.CheckCard(s); err != nil {
				t.Errorf("sample %+v fails its own check: %v", s, err)
			}
		}
	}
	machinetest.Invariants(t, m)
	broken := []discord.CardSnap{
		{Phase: discord.CardFailing},
		{Phase: discord.CardFailing, Failures: 1},
		{Phase: discord.CardCurrent, Failures: 1},
		{Phase: discord.CardLive, NextRetry: cardT0()},
		{Phase: discord.CardDirty, NextRetry: cardT0()},
		{Phase: discord.CardFailing, Failures: 1, NextRetry: cardT0(), RateUntil: cardT0().Add(time.Second)},
		{Phase: "bogus"},
	}
	for _, s := range broken {
		if discord.CheckCard(s) == nil {
			t.Errorf("check accepted %+v", s)
		}
	}
}

func cardFuzzPath(kinds ...discord.CardKind) []byte {
	events := []discord.CardEvent{}
	for _, k := range discord.CardKinds() {
		events = append(events, cardEvents(k)...)
	}
	out := make([]byte, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, byte(slices.IndexFunc(events, func(e discord.CardEvent) bool { return e.Kind == k })))
	}
	return out
}

func FuzzCardMachine(f *testing.F) {
	f.Add(cardFuzzPath(discord.CardMark, discord.CardShown, discord.CardMark))
	f.Add(cardFuzzPath(discord.CardMark, discord.CardFailed, discord.CardRetryDue, discord.CardFailed))
	f.Add(cardFuzzPath(discord.CardPress, discord.CardFailed, discord.CardPress, discord.CardHeld))
	f.Add(cardFuzzPath(discord.CardMark, discord.CardAllGone, discord.CardMark))
	for _, k := range discord.CardKinds() {
		f.Add(cardFuzzPath(k))
		f.Add(cardFuzzPath(discord.CardMark, k))
		f.Add(cardFuzzPath(discord.CardMark, discord.CardFailed, k))
	}
	m := cardMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func mustCard(t *testing.T, s discord.CardSnap, ev discord.CardEvent) discord.CardSnap {
	t.Helper()
	next, err := discord.NextCard(s, ev)
	if err != nil {
		t.Fatalf("%+v x %+v: %v", s, ev, err)
	}
	return next
}

func failAt(t *testing.T, s discord.CardSnap, at time.Time, retryAfter time.Duration) discord.CardSnap {
	t.Helper()
	s = mustCard(t, s, discord.CardEvent{Kind: discord.CardFailed, At: at, RetryAfter: retryAfter})
	if s.Phase != discord.CardFailing {
		t.Fatalf("after Failed: %+v", s)
	}
	return s
}

func TestCardMachine_BackoffDoublesToCap(t *testing.T) {
	t.Parallel()
	s := mustCard(
		t,
		discord.CardSnap{Phase: discord.CardCurrent},
		discord.CardEvent{Kind: discord.CardMark, At: cardT0()},
	)
	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		320 * time.Second, 640 * time.Second, 1280 * time.Second, 30 * time.Minute, 30 * time.Minute,
	}
	at := cardT0()
	for i, w := range want {
		s = failAt(t, s, at, 0)
		if got := s.NextRetry.Sub(at); got != w || s.Failures != i+1 {
			t.Fatalf("failure %d: next retry in %v (failures %d), want %v", i+1, got, s.Failures, w)
		}
		at = s.NextRetry
		s = mustCard(t, s, discord.CardEvent{Kind: discord.CardRetryDue, At: at})
	}
	if got := discord.CardBackoff(200, discord.CardRetry{}); got != 30*time.Minute {
		t.Errorf("backoff(200) = %v, want the cap", got)
	}
	custom := discord.CardRetry{First: 200 * time.Millisecond, Cap: time.Second}
	if got := discord.CardBackoff(2, custom); got != 400*time.Millisecond {
		t.Errorf("custom backoff(2) = %v, want 400ms", got)
	}
	if got := discord.CardBackoff(5, custom); got != time.Second {
		t.Errorf("custom backoff(5) = %v, want the 1s cap", got)
	}
}

func TestCardMachine_RetryAfterFloorsBackoff(t *testing.T) {
	t.Parallel()
	dirty := discord.CardSnap{Phase: discord.CardDirty}
	s := failAt(t, dirty, cardT0(), time.Hour)
	if !s.NextRetry.Equal(cardT0().Add(time.Hour)) || !s.RateUntil.Equal(cardT0().Add(time.Hour)) {
		t.Errorf("429 for 1h: %+v", s)
	}
	s = failAt(t, dirty, cardT0(), time.Second)
	if !s.NextRetry.Equal(cardT0().Add(10*time.Second)) || !s.RateUntil.Equal(cardT0().Add(time.Second)) {
		t.Errorf("429 for 1s: %+v, want backoff 10s and rate until 1s", s)
	}
	s = failAt(t, dirty, cardT0(), 0)
	if !s.RateUntil.IsZero() {
		t.Errorf("plain failure sets RateUntil: %+v", s)
	}
}

func TestCardMachine_PressBypassesBackoffNotRateLimit(t *testing.T) {
	t.Parallel()
	plain := failAt(t, discord.CardSnap{Phase: discord.CardDirty}, cardT0(), 0)
	if got := mustCard(
		t,
		plain,
		discord.CardEvent{Kind: discord.CardPress, At: cardT0().Add(time.Second)},
	); got.Phase !=
		discord.CardDirty || got.Failures != 1 ||
		!got.NextRetry.IsZero() {
		t.Errorf("press during plain backoff = %+v, want dirty keeping failures", got)
	}
	limited := failAt(t, discord.CardSnap{Phase: discord.CardDirty}, cardT0(), time.Minute)
	if got := mustCard(t, limited, discord.CardEvent{Kind: discord.CardPress, At: cardT0().Add(time.Second)}); got !=
		limited {
		t.Errorf("press before Retry-After = %+v, want unchanged %+v", got, limited)
	}
	if got := mustCard(
		t,
		limited,
		discord.CardEvent{Kind: discord.CardPress, At: cardT0().Add(time.Minute)},
	); got.Phase !=
		discord.CardDirty ||
		!got.RateUntil.IsZero() {
		t.Errorf("press after Retry-After = %+v, want dirty", got)
	}
}

func TestCardMachine_MarkDefersWhileFailing(t *testing.T) {
	t.Parallel()
	s := failAt(t, discord.CardSnap{Phase: discord.CardDirty}, cardT0(), 0)
	for _, k := range []discord.CardKind{discord.CardMark} {
		if got := mustCard(t, s, discord.CardEvent{Kind: k, At: cardT0().Add(time.Hour)}); got != s {
			t.Errorf("%v while failing = %+v, want unchanged", k, got)
		}
	}
}

func TestCardMachine_RetryDueClearsNextRetry(t *testing.T) {
	t.Parallel()
	s := failAt(t, discord.CardSnap{Phase: discord.CardDirty, Failures: 2}, cardT0(), time.Second)
	_, err := discord.NextCard(s, discord.CardEvent{Kind: discord.CardRetryDue, At: s.NextRetry.Add(-time.Millisecond)})

	if _, ok := errors.AsType[*discord.RejectedError](err); !ok {
		t.Errorf("early RetryDue = %v, want a rejection", err)
	}
	got := mustCard(t, s, discord.CardEvent{Kind: discord.CardRetryDue, At: s.NextRetry})
	if got.Phase != discord.CardDirty || got.Failures != 3 || !got.NextRetry.IsZero() || !got.RateUntil.IsZero() {
		t.Errorf("RetryDue = %+v, want dirty with failures 3 and no instants", got)
	}
	shown := mustCard(t, got, discord.CardEvent{Kind: discord.CardShown, At: cardT0(), Live: true})
	if shown != (discord.CardSnap{Phase: discord.CardLive}) {
		t.Errorf("recovery = %+v, want live with failures 0", shown)
	}
}

func TestMachineDiagram_Card(t *testing.T) {
	t.Parallel()
	machinetest.GoldenMermaid(t, cardMachine(), "testdata/card.md")
}
