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
//
// Every per-run context is cancelled when shutdown closes. That's relayed by one
// goroutine per run rather than derived directly from a stored base
// [context.Context]: holding a long-lived context in a struct field is exactly
// the pattern that invites stale/wrong-context bugs (a context meant for one
// call silently reused far outside its original scope) — a channel that's
// closed exactly once is a plainer, harder-to-misuse "the process is shutting
// down" signal, and newRunManager only ever reads from the context passed to
// it, never stores it.
type runManager struct {
	mu       sync.Mutex
	active   map[string]*runToken
	shutdown chan struct{}

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

// newRunManager builds a runManager whose runs are all cancelled once base is
// done (e.g. the process-lifetime context from signal.NotifyContext). base is
// read from exactly once, by a single goroutine that relays its Done() into the
// shutdown channel every per-run context selects on — see runManager's doc
// comment for why that indirection exists instead of storing base itself.
func newRunManager(base context.Context) *runManager {
	m := &runManager{
		active:     make(map[string]*runToken),
		shutdown:   make(chan struct{}),
		globalSlot: make(chan struct{}, 1),
	}
	m.globalSlot <- struct{}{}
	go func() {
		<-base.Done()
		close(m.shutdown)
	}()
	return m
}

// begin cancels any in-flight run for id and registers a fresh cancellable
// context — cancelled either by a superseding begin() for the same id, or by
// the manager's shutdown, whichever comes first.
func (m *runManager) begin(id string) (context.Context, *runToken) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.active[id]; ok {
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-m.shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
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
