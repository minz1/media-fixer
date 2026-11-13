package machinetest_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/machinetest"
)

const (
	doorClosed  = "closed"
	doorOpen    = "open"
	doorLocked  = "locked"
	doorGone    = "gone"
	doorStuck   = "stuck"
	doorPainted = "painted"

	kindPush   = "push"
	kindPull   = "pull"
	kindLock   = "lock"
	kindUnlock = "unlock"
	kindRemove = "remove"
	kindJam    = "jam"
)

type recorder struct {
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) has(sub string) bool {
	return slices.ContainsFunc(r.errs, func(e string) bool { return strings.Contains(e, sub) })
}

type door struct {
	State string
}

type doorEv struct {
	Kind string
}

type doorRow struct {
	from, kind, to string
	reject         bool
}

func doorStates() []string { return []string{doorClosed, doorOpen, doorLocked, doorGone} }

func doorKinds() []string { return []string{kindPush, kindPull, kindLock, kindUnlock, kindRemove} }

func doorAllowed() []doorRow {
	return []doorRow{
		{from: doorClosed, kind: kindPush, to: doorOpen},
		{from: doorOpen, kind: kindPull, to: doorClosed},
		{from: doorClosed, kind: kindLock, to: doorLocked},
		{from: doorLocked, kind: kindUnlock, to: doorClosed},
		{from: doorClosed, kind: kindRemove, to: doorGone},
	}
}

func withRejects(states, kinds []string, allowed []doorRow) []doorRow {
	rows := slices.Clone(allowed)
	for _, s := range states {
		for _, k := range kinds {
			if !slices.ContainsFunc(allowed, func(r doorRow) bool { return r.from == s && r.kind == k }) {
				rows = append(rows, doorRow{from: s, kind: k, reject: true})
			}
		}
	}
	return rows
}

func doorMachine(states, kinds []string, rows []doorRow) machinetest.Machine[string, string, door, doorEv] {
	return machinetest.Machine[string, string, door, doorEv]{
		States:  states,
		Kinds:   kinds,
		Initial: door{State: doorClosed},
		State:   func(d door) string { return d.State },
		Key:     func(d door) string { return d.State },
		Samples: func(s string) []door { return []door{{State: s}} },
		Events:  func(k string) []doorEv { return []doorEv{{Kind: k}} },
		Matches: func(d door, ev doorEv) int {
			n := 0
			for _, r := range rows {
				if r.from == d.State && r.kind == ev.Kind {
					n++
				}
			}
			return n
		},
		Next: func(d door, ev doorEv) (door, error) {
			for _, r := range rows {
				if r.from != d.State || r.kind != ev.Kind {
					continue
				}
				if r.reject {
					return d, errors.New("rejected")
				}
				return door{State: r.to}, nil
			}
			return d, errors.New("no row")
		},
		Check: func(d door) error {
			if slices.Contains(states, d.State) {
				return nil
			}
			return fmt.Errorf("unknown state %q", d.State)
		},
		Terminal: func(s string) bool { return s == doorGone },
	}
}

func goodDoor() machinetest.Machine[string, string, door, doorEv] {
	return doorMachine(doorStates(), doorKinds(), withRejects(doorStates(), doorKinds(), doorAllowed()))
}

func allBytes() []byte {
	data := make([]byte, 0, 256)
	for i := range 256 {
		data = append(data, byte(i))
	}
	return data
}

func TestTotality_PassesOnCompleteTable(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	m := goodDoor()
	machinetest.Totality(r, m)
	machinetest.Reachability(r, m)
	machinetest.Invariants(r, m)
	machinetest.Sequence(r, m, allBytes())
	if len(r.errs) != 0 {
		t.Fatalf("complete door machine flagged: %v", r.errs)
	}
}

func TestTotality_FlagsMissingPair(t *testing.T) {
	t.Parallel()
	rows := withRejects(doorStates(), doorKinds(), doorAllowed())
	rows = slices.DeleteFunc(rows, func(r doorRow) bool { return r.from == doorOpen && r.kind == kindLock })
	r := &recorder{}
	machinetest.Totality(r, doorMachine(doorStates(), doorKinds(), rows))
	if len(r.errs) != 1 || !r.has("state open, kind lock: 0 rows") {
		t.Fatalf("errors = %v, want one for open x lock", r.errs)
	}
}

func TestTotality_FlagsOverlappingRows(t *testing.T) {
	t.Parallel()
	rows := withRejects(doorStates(), doorKinds(), doorAllowed())
	rows = append(rows, doorRow{from: doorClosed, kind: kindPush, to: doorLocked})
	r := &recorder{}
	machinetest.Totality(r, doorMachine(doorStates(), doorKinds(), rows))
	if len(r.errs) != 1 || !r.has("state closed, kind push: 2 rows") {
		t.Fatalf("errors = %v, want one for closed x push", r.errs)
	}
}

func TestReachability_FlagsUnreachableState(t *testing.T) {
	t.Parallel()
	states := append(doorStates(), doorPainted)
	r := &recorder{}
	machinetest.Reachability(r, doorMachine(states, doorKinds(), withRejects(states, doorKinds(), doorAllowed())))
	if len(r.errs) != 1 || !r.has("state painted is unreachable") {
		t.Fatalf("errors = %v, want painted unreachable", r.errs)
	}
}

func TestReachability_FlagsDeadEnd(t *testing.T) {
	t.Parallel()
	states := append(doorStates(), doorStuck)
	kinds := append(doorKinds(), kindJam)
	allowed := append(doorAllowed(), doorRow{from: doorOpen, kind: kindJam, to: doorStuck})
	r := &recorder{}
	machinetest.Reachability(r, doorMachine(states, kinds, withRejects(states, kinds, allowed)))
	if len(r.errs) != 1 || !r.has("snapshot stuck (state stuck) has no path to a terminal state") {
		t.Fatalf("errors = %v, want stuck dead end", r.errs)
	}
}

func TestReachability_NoTerminalNeedsEdgeToAnotherState(t *testing.T) {
	t.Parallel()
	states := []string{doorClosed, doorOpen, doorStuck}
	kinds := []string{kindPush, kindPull, kindJam}
	allowed := []doorRow{
		{from: doorClosed, kind: kindPush, to: doorOpen},
		{from: doorOpen, kind: kindPull, to: doorClosed},
		{from: doorOpen, kind: kindJam, to: doorStuck},
		{from: doorStuck, kind: kindJam, to: doorStuck},
	}
	m := doorMachine(states, kinds, withRejects(states, kinds, allowed))
	m.Terminal = func(string) bool { return false }
	r := &recorder{}
	machinetest.Reachability(r, m)
	if len(r.errs) != 1 || !r.has("snapshot stuck (state stuck) has no edge to another state") {
		t.Fatalf("errors = %v, want stuck without an exit edge", r.errs)
	}
}

func TestInvariants_FlagsBrokenTransition(t *testing.T) {
	t.Parallel()
	allowed := doorAllowed()
	allowed[1].to = "ajar"
	r := &recorder{}
	machinetest.Invariants(r, doorMachine(doorStates(), doorKinds(), withRejects(doorStates(), doorKinds(), allowed)))
	if len(r.errs) != 1 || !r.has(`open -> ajar breaks an invariant: unknown state "ajar"`) {
		t.Fatalf("errors = %v, want open -> ajar flagged", r.errs)
	}
}

func TestInvariants_FlagsBrokenEffectObligation(t *testing.T) {
	t.Parallel()
	m := goodDoor()
	m.Extra = func(before door, ev doorEv) error {
		if before.State == doorClosed && ev.Kind == kindRemove {
			return errors.New("remove emitted no finish")
		}
		return nil
	}
	r := &recorder{}
	machinetest.Invariants(r, m)
	if len(r.errs) != 1 || !r.has("effect obligation: remove emitted no finish") {
		t.Fatalf("errors = %v, want the obligation flagged", r.errs)
	}
}

func TestSequence_FlagsPanicAndBrokenCheck(t *testing.T) {
	t.Parallel()
	m := goodDoor()
	next := m.Next
	m.Next = func(d door, ev doorEv) (door, error) {
		if d.State == doorOpen && ev.Kind == kindPull {
			panic("hinge fell off")
		}
		return next(d, ev)
	}
	r := &recorder{}
	machinetest.Sequence(r, m, []byte{0, 1})
	if len(r.errs) != 1 || !r.has("step 1") || !r.has("panic: hinge fell off") {
		t.Fatalf("errors = %v, want the panic at step 1", r.errs)
	}

	m = goodDoor()
	m.Check = func(d door) error {
		if d.State == doorLocked {
			return errors.New("locked is forbidden")
		}
		return nil
	}
	r = &recorder{}
	machinetest.Sequence(r, m, []byte{2})
	if len(r.errs) != 1 || !r.has("closed -> locked breaks an invariant: locked is forbidden") {
		t.Fatalf("errors = %v, want the broken check at step 0", r.errs)
	}
}

func TestSequence_RejectLeavesSnapshotUnchanged(t *testing.T) {
	t.Parallel()
	m := goodDoor()
	next := m.Next
	m.Next = func(d door, ev doorEv) (door, error) {
		got, err := next(d, ev)
		if err != nil {
			return door{State: doorGone}, err
		}
		return got, nil
	}
	r := &recorder{}
	machinetest.Sequence(r, m, []byte{1})
	if len(r.errs) != 1 || !r.has("rejected but changed closed to gone") {
		t.Fatalf("errors = %v, want the changed reject flagged", r.errs)
	}
}

type cell struct {
	V int
}

type cellEv struct {
	Kind string
}

func cellMachine(step func(cell, string) (cell, error)) machinetest.Machine[string, string, cell, cellEv] {
	kinds := []string{"inc", "set1", "set2"}
	return machinetest.Machine[string, string, cell, cellEv]{
		States:   []string{"on"},
		Kinds:    kinds,
		Initial:  cell{},
		State:    func(cell) string { return "on" },
		Key:      func(c cell) string { return strconv.Itoa(c.V) },
		Samples:  func(string) []cell { return []cell{{}} },
		Events:   func(k string) []cellEv { return []cellEv{{Kind: k}} },
		Matches:  func(cell, cellEv) int { return 1 },
		Next:     func(c cell, ev cellEv) (cell, error) { return step(c, ev.Kind) },
		Check:    func(cell) error { return nil },
		Terminal: func(string) bool { return false },
	}
}

func cellStep(c cell, kind string) (cell, error) {
	switch kind {
	case "inc":
		if c.V >= 3 {
			return c, errors.New("full")
		}
		return cell{V: c.V + 1}, nil
	case "set1":
		return cell{V: 1}, nil
	case "set2":
		return cell{V: 2}, nil
	}
	return c, errors.New("no row")
}

func TestIdempotence_FlagsCounter(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	machinetest.Idempotence(r, cellMachine(cellStep), []string{"inc"})
	if !r.has("inc") || !r.has("not idempotent") {
		t.Fatalf("errors = %v, want inc flagged", r.errs)
	}
	r = &recorder{}
	machinetest.Idempotence(r, cellMachine(cellStep), []string{"set1", "set2"})
	if len(r.errs) != 0 {
		t.Fatalf("set events flagged: %v", r.errs)
	}
}

func TestCommutes_FlagsOrder(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	machinetest.Commutes(r, cellMachine(cellStep), [][2]string{{"set1", "set2"}})
	if !r.has("set1") || !r.has("do not commute") {
		t.Fatalf("errors = %v, want set1/set2 flagged", r.errs)
	}
	r = &recorder{}
	machinetest.Commutes(r, cellMachine(cellStep), [][2]string{{"set1", "set1"}})
	if len(r.errs) != 0 {
		t.Fatalf("set1/set1 flagged: %v", r.errs)
	}
}

func TestIdempotence_DeliversOnceAndComparesExact(t *testing.T) {
	t.Parallel()
	m := cellMachine(func(c cell, kind string) (cell, error) {
		if kind == "set1" && c.V == 1 {
			return c, errors.New("stale")
		}
		return cellStep(c, kind)
	})
	m.Key = func(cell) string { return "one" }
	r := &recorder{}
	machinetest.Idempotence(r, m, []string{"set1"})
	if len(r.errs) != 0 {
		t.Fatalf("errors = %v, want none without Exact", r.errs)
	}
	m.Exact = func(c cell) string { return strconv.Itoa(c.V) }
	m.Deliver = func(cell, cellEv) cellEv { return cellEv{Kind: "inc"} }
	r = &recorder{}
	machinetest.Idempotence(r, m, []string{"set1"})
	if !r.has("not idempotent") {
		t.Fatalf("errors = %v, want the delivered inc flagged by Exact", r.errs)
	}
}

func TestIdempotence_FlagsUnexercisedKind(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	machinetest.Idempotence(r, cellMachine(cellStep), []string{"never"})
	if !r.has("never") || !r.has("nothing was checked") {
		t.Fatalf("errors = %v, want the kind that never applies flagged", r.errs)
	}
	refuseTwice := cellMachine(func(c cell, kind string) (cell, error) {
		if kind == "once" && c.V == 0 {
			return cell{V: 1}, nil
		}
		return c, errors.New("refused")
	})
	r = &recorder{}
	machinetest.Idempotence(r, refuseTwice, []string{"once"})
	if len(r.errs) != 0 {
		t.Fatalf("a refused second delivery flagged: %v", r.errs)
	}
}

func TestCommutes_FlagsVacuousPair(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	machinetest.Commutes(r, cellMachine(cellStep), [][2]string{{"set1", "never"}})
	if !r.has("set1") || !r.has("nothing was checked") {
		t.Fatalf("errors = %v, want the pair that never applies in both orders flagged", r.errs)
	}
}

func TestCommutes_DeliversBothEventsAtTheStart(t *testing.T) {
	t.Parallel()
	m := cellMachine(cellStep)
	m.Deliver = func(_ cell, ev cellEv) cellEv {
		return cellEv{Kind: map[string]string{"a": "set1", "b": "set2"}[ev.Kind]}
	}
	r := &recorder{}
	machinetest.Commutes(r, m, [][2]string{{"a", "b"}})
	if !r.has("do not commute") {
		t.Fatalf("errors = %v, want the delivered set1/set2 flagged", r.errs)
	}
}
