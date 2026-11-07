package agent

import (
	"context"
	"errors"
	"time"
)

type Scope struct {
	IncidentID string
	What       string
	ReportedAt time.Time
}

type toolCall struct {
	scope         Scope
	gate          Gate
	tool          string
	readyTimeout  time.Duration
	readyInterval time.Duration

	acted     bool
	params    map[string]any
	verdict   Verdict
	gateErr   error
	sideBound time.Duration
	side      context.Context
	release   context.CancelFunc
	cut       bool
}

type refusal struct {
	reason string
	reply  any
	cause  error
}

func (r *refusal) Error() string { return r.reason }

func (r *refusal) Unwrap() error { return r.cause }

func refuse(reason string) *refusal { return &refusal{reason: reason} }

func refuseWith(reason string, reply any) *refusal { return &refusal{reason: reason, reply: reply} }

func refuseCause(cause error) *refusal { return &refusal{reason: cause.Error(), cause: cause} }

var errAppliedNotReady = errors.New("applied but not ready")

type notReadyError struct{ err error }

func (e *notReadyError) Error() string { return e.err.Error() }

func (e *notReadyError) Unwrap() []error { return []error{errAppliedNotReady, e.err} }

func notReady(err error) error { return &notReadyError{err: err} }

var errActDenied = errors.New("action not allowed")

func (c *toolCall) act(ctx context.Context, params map[string]any) (context.Context, error) {
	c.acted = true
	c.params = params
	if c.gate == nil {
		c.verdict = GateAllowed
		return c.sideEffect(ctx), nil
	}
	c.verdict, c.gateErr = c.gate.Act(ctx, c.tool, params)
	if c.gateErr != nil {
		return ctx, c.gateErr
	}
	if c.verdict != GateAllowed {
		return ctx, errActDenied
	}
	return c.sideEffect(ctx), nil
}

func (c *toolCall) sideEffect(ctx context.Context) context.Context {
	bound := c.sideBound
	if bound == 0 {
		spec, _ := specByName(c.tool)
		bound = spec.bound()
	}
	c.side, c.release = context.WithTimeout(context.WithoutCancel(ctx), bound)
	return c.side
}

func (c *toolCall) finish() {
	if c.release == nil {
		return
	}
	c.cut = errors.Is(c.side.Err(), context.DeadlineExceeded)
	c.release()
}

func (c *toolCall) ready() (time.Duration, time.Duration) {
	timeout, interval := c.readyTimeout, c.readyInterval
	if timeout == 0 {
		timeout = restartReadyTimeout
	}
	if interval == 0 {
		interval = restartReadyInterval
	}
	return timeout, interval
}
