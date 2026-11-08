package discord

import (
	"errors"
	"fmt"
	"time"
)

type ConnState string

const (
	ConnIdle         ConnState = "idle"
	ConnConnecting   ConnState = "connecting"
	ConnBackoff      ConnState = "backoff"
	ConnConnected    ConnState = "connected"
	ConnReconnecting ConnState = "reconnecting"
	ConnClosed       ConnState = "closed"
)

func connStates() []ConnState {
	return []ConnState{ConnIdle, ConnConnecting, ConnBackoff, ConnConnected, ConnReconnecting, ConnClosed}
}

type Conn struct {
	State     ConnState
	Attempt   int
	Since     time.Time
	DownSince time.Time
	RetryAt   time.Time
	Err       string
}

type connKind string

const (
	connStart       connKind = "start"
	connOpenOK      connKind = "open_ok"
	connOpenFailed  connKind = "open_failed"
	connRetryDue    connKind = "retry_due"
	connGatewayDown connKind = "gateway_down"
	connGatewayUp   connKind = "gateway_up"
	connStop        connKind = "stop"
)

func connKinds() []connKind {
	return []connKind{connStart, connOpenOK, connOpenFailed, connRetryDue, connGatewayDown, connGatewayUp, connStop}
}

type connEvent struct {
	Kind connKind
	At   time.Time
	Err  string
	Wait time.Duration
}

const connErrUnknown = "open failed"

type connRule func(Conn, connEvent) (Conn, bool)

func connTo(s ConnState) connRule {
	return func(c Conn, ev connEvent) (Conn, bool) { return enterConn(c, s, ev.At), true }
}

func connStay(c Conn, _ connEvent) (Conn, bool) { return c, true }

func connReject(c Conn, _ connEvent) (Conn, bool) { return c, false }

func connStartRule(c Conn, ev connEvent) (Conn, bool) {
	next := enterConn(c, ConnConnecting, ev.At)
	next.Attempt, next.DownSince = 1, ev.At
	return next, true
}

func connOpenFailedRule(c Conn, ev connEvent) (Conn, bool) {
	next := enterConn(c, ConnBackoff, ev.At)
	next.RetryAt, next.Err = ev.At.Add(ev.Wait), ev.Err
	if next.Err == "" {
		next.Err = connErrUnknown
	}
	return next, true
}

func connRetryRule(c Conn, ev connEvent) (Conn, bool) {
	next := enterConn(c, ConnConnecting, ev.At)
	next.Attempt, next.RetryAt = c.Attempt+1, time.Time{}
	return next, true
}

func connDropRule(c Conn, ev connEvent) (Conn, bool) {
	next := enterConn(c, ConnReconnecting, ev.At)
	next.DownSince = ev.At
	return next, true
}

func enterConn(c Conn, s ConnState, at time.Time) Conn {
	next := c
	next.State, next.Since = s, at
	switch s {
	case ConnConnected:
		next.Attempt, next.Err, next.RetryAt, next.DownSince = 0, "", time.Time{}, time.Time{}
	case ConnClosed:
		next.Attempt, next.RetryAt, next.DownSince = 0, time.Time{}, time.Time{}
	case ConnIdle, ConnConnecting, ConnBackoff, ConnReconnecting:
	}
	return next
}

func connTransitions() map[ConnState]map[connKind]connRule {
	closed, up, rej := connTo(ConnClosed), connTo(ConnConnected), connReject
	return map[ConnState]map[connKind]connRule{
		ConnIdle: {
			connStart: connStartRule, connOpenOK: rej, connOpenFailed: rej, connRetryDue: rej,
			connGatewayDown: rej, connGatewayUp: rej, connStop: closed,
		},
		ConnConnecting: {
			connStart: rej, connOpenOK: up, connOpenFailed: connOpenFailedRule, connRetryDue: rej,
			connGatewayDown: connStay, connGatewayUp: connStay, connStop: closed,
		},
		ConnBackoff: {
			connStart: rej, connOpenOK: rej, connOpenFailed: rej, connRetryDue: connRetryRule,
			connGatewayDown: rej, connGatewayUp: rej, connStop: closed,
		},
		ConnConnected: {
			connStart: rej, connOpenOK: rej, connOpenFailed: rej, connRetryDue: rej,
			connGatewayDown: connDropRule, connGatewayUp: connStay, connStop: closed,
		},
		ConnReconnecting: {
			connStart: rej, connOpenOK: rej, connOpenFailed: rej, connRetryDue: rej,
			connGatewayDown: connStay, connGatewayUp: up, connStop: closed,
		},
		ConnClosed: {
			connStart: rej, connOpenOK: rej, connOpenFailed: rej, connRetryDue: rej,
			connGatewayDown: rej, connGatewayUp: rej, connStop: rej,
		},
	}
}

func connMatches(s ConnState, k connKind) int {
	if _, ok := connTransitions()[s][k]; ok {
		return 1
	}
	return 0
}

func nextConn(c Conn, ev connEvent) (Conn, error) {
	rule, ok := connTransitions()[c.State][ev.Kind]
	if !ok {
		return c, fmt.Errorf("discord gateway %s has no row for %s", c.State, ev.Kind)
	}
	next, accepted := rule(c, ev)
	if !accepted {
		return c, &rejectedError{kind: string(ev.Kind), from: "discord gateway " + string(c.State)}
	}
	return next, nil
}

var (
	errConnBackoff  = errors.New("backoff needs a retry time and an error")
	errConnAttempt  = errors.New("attempt does not match the state")
	errConnRetryAt  = errors.New("retry time outside backoff")
	errConnDownTime = errors.New("down-since does not match the state")
)

func checkConn(c Conn) error {
	var down, trying bool
	switch c.State {
	case ConnConnecting, ConnBackoff:
		down, trying = true, true
	case ConnReconnecting:
		down = true
	case ConnIdle, ConnConnected, ConnClosed:
	default:
		return fmt.Errorf("unknown discord gateway state %q", c.State)
	}
	if (c.State == ConnBackoff) != (!c.RetryAt.IsZero() && c.Err != "") {
		return fmt.Errorf("G1: %w: %+v", errConnBackoff, c)
	}
	if trying != (c.Attempt >= 1) || (!trying && c.Attempt != 0) {
		return fmt.Errorf("G2: %w: %+v", errConnAttempt, c)
	}
	if c.State != ConnBackoff && !c.RetryAt.IsZero() {
		return fmt.Errorf("G3: %w: %+v", errConnRetryAt, c)
	}
	if down != !c.DownSince.IsZero() {
		return fmt.Errorf("G4: %w: %+v", errConnDownTime, c)
	}
	return nil
}
