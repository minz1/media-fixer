package incident

import (
	"context"
	"sync"
	"time"
)

// runManager guarantees at most one active agent pipeline per incident, and — via
// globalSlot — that at most one incident's LLM diagnostic loop (or owner-approved
// escalation) is executing at any moment across the whole process. Starting a run
// for an incident cancels any run already in flight for it (cancel-on-supersede),
// so a reopen or reinvestigate never overlaps a prior run's still-sleeping
// verification loop — the root cause of the duplicate "fixed" notification.
type runManager struct {
	mu       sync.Mutex
	active   map[string]*runToken
	base     context.Context
	current  map[string]int64
	reserved map[string]int
	plans    map[string]int
	checks   map[string]armedCheck
	inflight map[string]*inflightCheck

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
	// wait and Phase 4's durable outcome tracking can run for
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
	gen        int64
}

type armedCheck struct {
	gen   int64
	timer *time.Timer
}

type inflightCheck struct {
	gen int64
}

func newRunManager(base context.Context) *runManager {
	m := &runManager{
		active:     make(map[string]*runToken),
		current:    make(map[string]int64),
		reserved:   make(map[string]int),
		plans:      make(map[string]int),
		checks:     make(map[string]armedCheck),
		inflight:   make(map[string]*inflightCheck),
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

func (m *runManager) supersedeGen(id string, gen int64) (context.Context, *runToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen < m.current[id] {
		return nil, nil, false
	}
	m.current[id] = gen
	if prev, ok := m.active[id]; ok {
		if prev.gen >= gen {
			return nil, nil, false
		}
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(m.base)
	tok := &runToken{cancel: cancel, gen: gen}
	m.active[id] = tok
	return ctx, tok, true
}

func (m *runManager) observe(id string, gen int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen > m.current[id] {
		m.current[id] = gen
	}
}

func (m *runManager) cancelBelow(id string, gen int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen > m.current[id] {
		m.current[id] = gen
	}
	if prev, ok := m.active[id]; ok && prev.gen < gen {
		delete(m.active, id)
		prev.cancel()
	}
	if c, ok := m.checks[id]; ok && c.gen < gen {
		c.timer.Stop()
		delete(m.checks, id)
	}
}

func (m *runManager) reserve(id string) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserved[id]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.reserved[id]--; m.reserved[id] <= 0 {
				delete(m.reserved, id)
			}
		})
	}
}

func (m *runManager) armCheck(id string, gen int64, at time.Time, fire func(done func())) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if gen < m.current[id] {
		return false
	}
	m.current[id] = gen
	if c, ok := m.checks[id]; ok {
		if c.gen >= gen {
			return false
		}
		c.timer.Stop()
	}
	m.checks[id] = armedCheck{gen: gen, timer: time.AfterFunc(max(time.Until(at), 0), func() {
		fire(m.claimCheck(id, gen))
	})}
	return true
}

func (m *runManager) claimCheck(id string, gen int64) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.checks[id]; ok && c.gen == gen {
		delete(m.checks, id)
	}
	if cur, ok := m.inflight[id]; ok && cur.gen > gen {
		return func() {}
	}
	mark := &inflightCheck{gen: gen}
	m.inflight[id] = mark
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.inflight[id] == mark {
				delete(m.inflight, id)
			}
		})
	}
}

type liveness struct {
	runs     map[string]int64
	reserved map[string]bool
	plans    map[string]bool
	armed    map[string]int64
	floors   map[string]int
}

func (m *runManager) liveness() liveness {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := liveness{
		runs: map[string]int64{}, reserved: map[string]bool{}, plans: map[string]bool{},
		armed: map[string]int64{}, floors: map[string]int{},
	}
	for id, tok := range m.active {
		l.runs[id], l.floors[id] = tok.gen, tok.staleFloor
	}
	for id := range m.reserved {
		l.reserved[id] = true
	}
	for id := range m.plans {
		l.plans[id] = true
	}
	for id, c := range m.inflight {
		l.armed[id] = c.gen
	}
	for id, c := range m.checks {
		l.armed[id] = max(l.armed[id], c.gen)
	}
	return l
}

func (m *runManager) currentGen(id string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current[id]
}

func (m *runManager) planStarted(id string) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans[id]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.plans[id]--; m.plans[id] <= 0 {
				delete(m.plans, id)
			}
		})
	}
}
