package incident

import (
	"context"
	"sync"
)

// runManager guarantees at most one active agent pipeline per incident, and — via
// globalSlot — that at most one incident's LLM diagnostic loop (or owner-approved
// escalation) is executing at any moment across the whole process. Starting a run
// for an incident cancels any run already in flight for it (cancel-on-supersede),
// so a reopen or reinvestigate never overlaps a prior run's still-sleeping
// verification loop — the root cause of the duplicate "fixed" notification. Every
// per-incident context descends from one base context, so cancelling the base (on
// shutdown) cancels all in-flight runs.
type runManager struct {
	base   context.Context //nolint:containedctx // base for all per-incident run contexts, set once at construction
	mu     sync.Mutex
	active map[string]*runToken

	// globalSlot serializes the disruptive part of concurrent incident work: the
	// LLM tool-calling diagnostic loop (Agent.Run) and owner-approved escalation
	// execution (RunEscalation) — the two places that actually call service-wide
	// autonomous actions (restart_jellyfin, jellyfin_library_scan,
	// decypharr_cache_cleanup, ...) or delete/re-search files. Without this, two
	// incidents diagnosing at once could have one's restart or scan corrupt the
	// evidence the other is reading mid-tool-call — confirmed plausible from
	// production logs, where a restart_jellyfin at 03:10:14–21 was followed 11s
	// later by a different incident's diagnosis concluding "not indexed" while
	// Jellyfin was still warming up.
	//
	// Deliberately NOT held across the whole run: a deferred fix's verification
	// wait (runVerification) and Phase 4's durable outcome tracking can run for
	// minutes to hours and must never block the queue — those poll read-only
	// state and don't need exclusivity. Diagnostic rounds are ~10s each and this
	// app sees roughly a handful of incidents a day, so full serialization of
	// just that part costs nothing observable while eliminating the whole
	// interference class. A single-buffered channel holding one token acts as a
	// context-cancellable mutex (sync.Mutex has no cancellable Lock).
	globalSlot chan struct{}
}

// runToken identifies a specific run so end() only clears the registration if a
// superseding begin() has not already replaced it.
type runToken struct{ cancel context.CancelFunc }

func newRunManager(base context.Context) *runManager {
	m := &runManager{base: base, active: make(map[string]*runToken), globalSlot: make(chan struct{}, 1)}
	m.globalSlot <- struct{}{}
	return m
}

// begin cancels any in-flight run for id and registers a fresh cancellable context.
func (m *runManager) begin(id string) (context.Context, *runToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.active[id]; ok {
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(m.base)
	tok := &runToken{cancel: cancel}
	m.active[id] = tok
	return ctx, tok
}

// end releases tok's context and clears the registration, but only if tok is still
// the current run for id (a superseding begin() may already have replaced it).
func (m *runManager) end(id string, tok *runToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.active[id]; ok && cur == tok {
		delete(m.active, id)
	}
	tok.cancel()
}

// acquireGlobal blocks until the single global diagnostic slot is free, or ctx is
// done first (e.g. this run was superseded while waiting).
func (m *runManager) acquireGlobal(ctx context.Context) error {
	select {
	case <-m.globalSlot:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseGlobal returns the global diagnostic slot. Must be called exactly once
// for every successful acquireGlobal.
func (m *runManager) releaseGlobal() {
	m.globalSlot <- struct{}{}
}
