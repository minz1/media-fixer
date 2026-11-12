package discord

import (
	"errors"
	"fmt"
	"time"
)

const (
	cardRetryFirstDefault = 10 * time.Second
	cardRetryCapDefault   = 30 * time.Minute
)

type cardPhase string

const (
	cardCurrent cardPhase = "current"
	cardDirty   cardPhase = "dirty"
	cardLive    cardPhase = "live"
	cardFailing cardPhase = "failing"
	cardGone    cardPhase = "gone"
)

func cardPhases() []cardPhase {
	return []cardPhase{cardCurrent, cardDirty, cardLive, cardFailing, cardGone}
}

type cardKind string

const (
	cardMark        cardKind = "mark"
	cardPress       cardKind = "press"
	cardRetryDue    cardKind = "retry_due"
	cardShown       cardKind = "shown"
	cardHeld        cardKind = "held"
	cardMissing     cardKind = "missing"
	cardUndecodable cardKind = "undecodable"
	cardAllGone     cardKind = "all_gone"
	cardFailed      cardKind = "failed"
	cardPruned      cardKind = "pruned"
)

func cardKinds() []cardKind {
	return []cardKind{
		cardMark, cardPress, cardRetryDue, cardShown, cardHeld, cardMissing, cardUndecodable, cardAllGone,
		cardFailed, cardPruned,
	}
}

type cardRetry struct {
	First time.Duration
	Cap   time.Duration
}

type cardEvent struct {
	Kind       cardKind
	At         time.Time
	Live       bool
	RetryAfter time.Duration
	Retry      cardRetry
}

type cardSnap struct {
	Phase     cardPhase
	Failures  int
	NextRetry time.Time
	RateUntil time.Time
}

type rejectedError struct {
	kind string
	from string
}

func (e *rejectedError) Error() string {
	return fmt.Sprintf("%s rejects %s", e.from, e.kind)
}

type cardRule func(cardSnap, cardEvent) (cardSnap, bool)

func cardTo(p cardPhase) cardRule {
	return func(s cardSnap, _ cardEvent) (cardSnap, bool) { return enterCard(s, p), true }
}

func cardStay(s cardSnap, _ cardEvent) (cardSnap, bool) { return s, true }

func cardReject(s cardSnap, _ cardEvent) (cardSnap, bool) { return s, false }

func cardShownRule(s cardSnap, ev cardEvent) (cardSnap, bool) {
	if ev.Live {
		return enterCard(s, cardLive), true
	}
	return enterCard(s, cardCurrent), true
}

func cardFailedRule(s cardSnap, ev cardEvent) (cardSnap, bool) {
	n := s.Failures + 1
	wait := max(cardBackoff(n, ev.Retry), ev.RetryAfter)
	next := cardSnap{Phase: cardFailing, Failures: n, NextRetry: ev.At.Add(wait)}
	if ev.RetryAfter > 0 {
		next.RateUntil = ev.At.Add(ev.RetryAfter)
	}
	return next, true
}

func cardPressWhileFailing(s cardSnap, ev cardEvent) (cardSnap, bool) {
	if ev.At.Before(s.RateUntil) {
		return s, true
	}
	return retryCard(s), true
}

func cardRetryDueWhileFailing(s cardSnap, ev cardEvent) (cardSnap, bool) {
	if ev.At.Before(s.NextRetry) {
		return s, false
	}
	return retryCard(s), true
}

func retryCard(s cardSnap) cardSnap {
	return cardSnap{Phase: cardDirty, Failures: s.Failures}
}

func enterCard(s cardSnap, p cardPhase) cardSnap {
	if p == cardDirty {
		return cardSnap{Phase: p, Failures: s.Failures}
	}
	return cardSnap{Phase: p}
}

func cardTransitions() map[cardPhase]map[cardKind]cardRule {
	dirty, gone, rej := cardTo(cardDirty), cardTo(cardGone), cardReject
	return map[cardPhase]map[cardKind]cardRule{
		cardCurrent: {
			cardMark: dirty, cardPress: dirty, cardRetryDue: rej,
			cardShown: rej, cardHeld: rej, cardMissing: rej, cardUndecodable: rej, cardAllGone: rej, cardFailed: rej,
			cardPruned: gone,
		},
		cardDirty: {
			cardMark: cardStay, cardPress: cardStay, cardRetryDue: rej,
			cardShown: cardShownRule, cardHeld: cardTo(cardCurrent), cardMissing: gone, cardUndecodable: gone,
			cardAllGone: gone, cardFailed: cardFailedRule, cardPruned: gone,
		},
		cardLive: {
			cardMark: dirty, cardPress: dirty, cardRetryDue: rej,
			cardShown: rej, cardHeld: rej, cardMissing: rej, cardUndecodable: rej, cardAllGone: rej, cardFailed: rej,
			cardPruned: gone,
		},
		cardFailing: {
			cardMark: cardStay, cardPress: cardPressWhileFailing,
			cardRetryDue: cardRetryDueWhileFailing,
			cardShown:    rej, cardHeld: rej, cardMissing: rej, cardUndecodable: rej, cardAllGone: rej, cardFailed: rej,
			cardPruned: gone,
		},
		cardGone: {
			cardMark: rej, cardPress: rej, cardRetryDue: rej,
			cardShown: rej, cardHeld: rej, cardMissing: rej, cardUndecodable: rej, cardAllGone: rej, cardFailed: rej,
			cardPruned: rej,
		},
	}
}

func cardMatches(p cardPhase, k cardKind) int {
	if _, ok := cardTransitions()[p][k]; ok {
		return 1
	}
	return 0
}

func nextCard(s cardSnap, ev cardEvent) (cardSnap, error) {
	rule, ok := cardTransitions()[s.Phase][ev.Kind]
	if !ok {
		return s, fmt.Errorf("card %s has no row for %s", s.Phase, ev.Kind)
	}
	next, accepted := rule(s, ev)
	if !accepted {
		return s, &rejectedError{kind: string(ev.Kind), from: "card " + string(s.Phase)}
	}
	return next, nil
}

func cardBackoff(n int, r cardRetry) time.Duration {
	first, ceiling := r.First, r.Cap
	if first <= 0 {
		first = cardRetryFirstDefault
	}
	if ceiling <= 0 {
		ceiling = cardRetryCapDefault
	}
	d := first
	for i := 1; i < n && d < ceiling; i++ {
		d += d
	}
	return min(d, ceiling)
}

var (
	errCardFailing = errors.New("failing needs failures and a next retry")
	errCardRate    = errors.New("rate limit outside failing or after the next retry")
	errCardSettled = errors.New("a settled card keeps no failures or instants")
	errCardDirty   = errors.New("a dirty card keeps no instants")
)

func checkCard(s cardSnap) error {
	failing := s.Failures >= 1 && !s.NextRetry.IsZero()
	if (s.Phase == cardFailing) != failing {
		return fmt.Errorf("C1: %w: %+v", errCardFailing, s)
	}
	if !s.RateUntil.IsZero() && (s.Phase != cardFailing || s.RateUntil.After(s.NextRetry)) {
		return fmt.Errorf("C2: %w: %+v", errCardRate, s)
	}
	switch s.Phase {
	case cardCurrent, cardLive, cardGone:
		if s.Failures != 0 || !s.NextRetry.IsZero() || !s.RateUntil.IsZero() {
			return fmt.Errorf("C3: %w: %+v", errCardSettled, s)
		}
	case cardDirty:
		if !s.NextRetry.IsZero() || !s.RateUntil.IsZero() {
			return fmt.Errorf("C4: %w: %+v", errCardDirty, s)
		}
	case cardFailing:
	default:
		return fmt.Errorf("unknown card phase %q", s.Phase)
	}
	return nil
}
