package machinetest

import (
	"fmt"
	"reflect"
)

const maxSnapshots = 1 << 16

type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

type Machine[S comparable, K comparable, Snap any, Ev any] struct {
	States   []S
	Kinds    []K
	Initial  Snap
	State    func(Snap) S
	Key      func(Snap) string
	Samples  func(S) []Snap
	Events   func(K) []Ev
	Matches  func(Snap, Ev) int
	Next     func(Snap, Ev) (Snap, error)
	Check    func(Snap) error
	Terminal func(S) bool
	Extra    func(before Snap, ev Ev) error
	Deliver  func(Snap, Ev) Ev
	Exact    func(Snap) string
}

func Totality[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev]) {
	t.Helper()
	for _, s := range m.States {
		for _, snap := range m.Samples(s) {
			for _, k := range m.Kinds {
				for _, ev := range m.Events(k) {
					if n := m.Matches(snap, ev); n != 1 {
						t.Errorf("state %v, kind %v: %d rows match %+v, want 1", s, k, n, ev)
					}
				}
			}
		}
	}
}

type graph[Snap any] struct {
	nodes map[string]Snap
	order []string
	edges map[string][]string
}

func explore[S, K comparable, Snap, Ev any](m Machine[S, K, Snap, Ev]) (graph[Snap], bool) {
	g := graph[Snap]{nodes: map[string]Snap{}, edges: map[string][]string{}}
	start := m.Key(m.Initial)
	g.nodes[start] = m.Initial
	g.order = append(g.order, start)
	for i := 0; i < len(g.order); i++ {
		key := g.order[i]
		for _, ev := range flatEvents(m) {
			next, err := m.Next(g.nodes[key], ev)
			if err != nil {
				continue
			}
			nk := m.Key(next)
			g.edges[key] = append(g.edges[key], nk)
			if _, seen := g.nodes[nk]; seen {
				continue
			}
			if len(g.order) >= maxSnapshots {
				return g, false
			}
			g.nodes[nk] = next
			g.order = append(g.order, nk)
		}
	}
	return g, true
}

func Reachability[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev]) {
	t.Helper()
	g, complete := explore(m)
	if !complete {
		t.Errorf("more than %d snapshots reachable; the search stopped", maxSnapshots)
		return
	}
	reached := map[S]bool{}
	for _, key := range g.order {
		reached[m.State(g.nodes[key])] = true
	}
	hasTerminal := false
	for _, s := range m.States {
		if !reached[s] {
			t.Errorf("state %v is unreachable from the initial snapshot", s)
		}
		hasTerminal = hasTerminal || m.Terminal(s)
	}
	if hasTerminal {
		checkExits(t, m, g)
		return
	}
	checkLeaves(t, m, g)
}

func Idempotence[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev], kinds []K) {
	t.Helper()
	g, complete := explore(m)
	if !complete {
		t.Errorf("more than %d snapshots reachable; the search stopped", maxSnapshots)
	}
	for _, key := range g.order {
		for _, k := range kinds {
			for _, ev := range m.Events(k) {
				if m.Deliver != nil {
					ev = m.Deliver(g.nodes[key], ev)
				}
				once, err := m.Next(g.nodes[key], ev)
				if err != nil {
					continue
				}
				twice, err := m.Next(once, ev)
				if err == nil && exact(m, twice) != exact(m, once) {
					t.Errorf("kind %v, event %+v from %s is not idempotent: %s then %s",
						k, ev, key, exact(m, once), exact(m, twice))
				}
			}
		}
	}
}

func exact[S, K comparable, Snap, Ev any](m Machine[S, K, Snap, Ev], s Snap) string {
	if m.Exact != nil {
		return m.Exact(s)
	}
	return m.Key(s)
}

func Commutes[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev], pairs [][2]K) {
	t.Helper()
	g, complete := explore(m)
	if !complete {
		t.Errorf("more than %d snapshots reachable; the search stopped", maxSnapshots)
	}
	for _, key := range g.order {
		for _, p := range pairs {
			for _, a := range m.Events(p[0]) {
				for _, b := range m.Events(p[1]) {
					ab, okAB := apply(m, g.nodes[key], a, b)
					ba, okBA := apply(m, g.nodes[key], b, a)
					if okAB && okBA && ab != ba {
						t.Errorf("kinds %v and %v, events %+v and %+v from %s do not commute: %s vs %s",
							p[0], p[1], a, b, key, ab, ba)
					}
				}
			}
		}
	}
}

func apply[S, K comparable, Snap, Ev any](m Machine[S, K, Snap, Ev], snap Snap, first, second Ev) (string, bool) {
	mid, err := m.Next(snap, first)
	if err != nil {
		return "", false
	}
	end, err := m.Next(mid, second)
	if err != nil {
		return "", false
	}
	return m.Key(end), true
}

func checkExits[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev], g graph[Snap]) {
	t.Helper()
	exits := map[string]bool{}
	for _, key := range g.order {
		exits[key] = m.Terminal(m.State(g.nodes[key]))
	}
	for changed := true; changed; {
		changed = false
		for _, key := range g.order {
			if exits[key] {
				continue
			}
			for _, to := range g.edges[key] {
				if exits[to] {
					exits[key] = true
					changed = true
					break
				}
			}
		}
	}
	for _, key := range g.order {
		if !exits[key] {
			t.Errorf("snapshot %s (state %v) has no path to a terminal state", key, m.State(g.nodes[key]))
		}
	}
}

func checkLeaves[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev], g graph[Snap]) {
	t.Helper()
	for _, key := range g.order {
		from := m.State(g.nodes[key])
		leaves := false
		for _, to := range g.edges[key] {
			if m.State(g.nodes[to]) != from {
				leaves = true
				break
			}
		}
		if !leaves {
			t.Errorf("snapshot %s (state %v) has no edge to another state", key, from)
		}
	}
}

func Invariants[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev]) {
	t.Helper()
	for _, s := range m.States {
		for _, snap := range m.Samples(s) {
			if m.Check(snap) == nil {
				checkFrom(t, m, s, snap)
			}
		}
	}
}

func checkFrom[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev], s S, snap Snap) {
	t.Helper()
	for _, ev := range flatEvents(m) {
		next, err := m.Next(snap, ev)
		if err != nil {
			continue
		}
		if cerr := m.Check(next); cerr != nil {
			t.Errorf("state %v, event %+v: %s -> %s breaks an invariant: %v", s, ev, m.Key(snap), m.Key(next), cerr)
		}
		if xerr := extra(m, snap, ev); xerr != nil {
			t.Errorf("state %v, event %+v: %v", s, ev, xerr)
		}
	}
}

func Sequence[S, K comparable, Snap, Ev any](t TB, m Machine[S, K, Snap, Ev], data []byte) {
	t.Helper()
	events := flatEvents(m)
	if len(events) == 0 {
		return
	}
	snap := m.Initial
	for i, b := range data {
		ev := events[int(b)%len(events)]
		next, panicked, err := safeNext(m, snap, ev)
		if panicked != nil {
			t.Errorf("step %d, event %+v from %s: panic: %v", i, ev, m.Key(snap), panicked)
			return
		}
		if err != nil {
			if !reflect.DeepEqual(next, snap) {
				t.Errorf("step %d, event %+v: rejected but changed %s to %s", i, ev, m.Key(snap), m.Key(next))
				return
			}
			continue
		}
		if cerr := m.Check(next); cerr != nil {
			t.Errorf("step %d, event %+v: %s -> %s breaks an invariant: %v", i, ev, m.Key(snap), m.Key(next), cerr)
			return
		}
		if xerr := extra(m, snap, ev); xerr != nil {
			t.Errorf("step %d, event %+v: %v", i, ev, xerr)
			return
		}
		snap = next
	}
}

func safeNext[S, K comparable, Snap, Ev any](
	m Machine[S, K, Snap, Ev], snap Snap, ev Ev,
) (Snap, any, error) {
	var (
		next     Snap
		err      error
		panicked any
	)
	func() {
		defer func() { panicked = recover() }()
		next, err = m.Next(snap, ev)
	}()
	return next, panicked, err
}

func extra[S, K comparable, Snap, Ev any](m Machine[S, K, Snap, Ev], before Snap, ev Ev) error {
	if m.Extra == nil {
		return nil
	}
	if err := m.Extra(before, ev); err != nil {
		return fmt.Errorf("effect obligation: %w", err)
	}
	return nil
}

func flatEvents[S, K comparable, Snap, Ev any](m Machine[S, K, Snap, Ev]) []Ev {
	var out []Ev
	for _, k := range m.Kinds {
		out = append(out, m.Events(k)...)
	}
	return out
}
