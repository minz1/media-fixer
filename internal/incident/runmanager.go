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
// verification loop — the root cause of the duplicate "fixed" notification.
type runManager struct {
	mu     sync.Mutex
	active map[string]*runToken
	base   context.Context

	// globalSlot serializes the disruptive part of concurrent incident work: the
	// LLM tool-calling diagnostic loop (Agent.Run) and owner-approved escalation
	// execution (ExecuteApprovedPlan) — the two places that actually call service-wide
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
// later supersede() has not already replaced it.
type runToken struct {
	cancel     context.CancelFunc
	staleFloor int
}

func newRunManager(base context.Context) *runManager {
	m := &runManager{
		active:     make(map[string]*runToken),
		base:       base,
		globalSlot: make(chan struct{}, 1),
	}
	m.globalSlot <- struct{}{}
	return m
}

// supersede cancels any in-flight run for id and registers a fresh cancellable
// context — cancelled either by a later supersede() for the same id, or by
// the manager's shutdown, whichever comes first.
func (m *runManager) supersede(id string) (context.Context, *runToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, replaced := m.active[id]
	if replaced {
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(m.base)
	tok := &runToken{cancel: cancel}
	m.active[id] = tok
	return ctx, tok, replaced
}

// end releases tok's context and clears the registration, but only if tok is still
// the current run for id (a later supersede() may already have replaced it).
func (m *runManager) end(id string, tok *runToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.active[id]; ok && cur == tok {
		delete(m.active, id)
	}
	tok.cancel()
}

func (m *runManager) running(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[id]
	return ok
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

// setStaleFloor keeps a run sleeping on its retry waits from being judged by lowered saved timers.
func (m *runManager) setStaleFloor(tok *runToken, minutes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok.staleFloor = minutes
}

func (m *runManager) staleFloor(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tok, ok := m.active[id]; ok {
		return tok.staleFloor
	}
	return 0
}
