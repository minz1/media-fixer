package incident

import (
	"context"
	"sync"
)

// runManager guarantees at most one active agent pipeline per incident. Starting a
// run for an incident cancels any run already in flight for it (cancel-on-supersede),
// so a reopen or reinvestigate never overlaps a prior run's still-sleeping
// verification loop — the root cause of the duplicate "fixed" notification. Every
// per-incident context descends from one base context, so cancelling the base (on
// shutdown) cancels all in-flight runs.
type runManager struct {
	base   context.Context //nolint:containedctx // base for all per-incident run contexts, set once at construction
	mu     sync.Mutex
	active map[string]*runToken
}

// runToken identifies a specific run so end() only clears the registration if a
// superseding begin() has not already replaced it.
type runToken struct{ cancel context.CancelFunc }

func newRunManager(base context.Context) *runManager {
	return &runManager{base: base, active: make(map[string]*runToken)}
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
