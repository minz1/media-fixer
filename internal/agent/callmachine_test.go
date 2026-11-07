package agent_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/machinetest"
)

func callImpacts() []agent.ToolImpactForTest {
	return []agent.ToolImpactForTest{agent.ImpactNone, agent.ImpactItem, agent.ImpactShared, agent.ImpactHeavy}
}

func callSamples(s agent.CallState) []agent.CallSnap {
	var out []agent.CallSnap
	for _, imp := range callImpacts() {
		snap := agent.CallSnap{State: s, Impact: imp, Counted: agent.CallMeta(s).Counted}
		snap.Disruptive = imp >= agent.ImpactShared && (s == agent.CallApplied || s == agent.CallFailed)
		out = append(out, snap)
	}
	return out
}

func callEvents(k agent.CallKind) []agent.CallKind { return []agent.CallKind{k} }

func hasEffect(effs []agent.CallEffect, want agent.CallEffect) bool {
	return slices.Contains(effs, want)
}

func callObligations(before agent.CallSnap, kind agent.CallKind) error {
	next, effs, err := agent.NextCall(before, kind)
	if err == nil {
		return callEffectObligations(before, next, effs)
	}
	return nil
}

func callEffectObligations(before, next agent.CallSnap, effs []agent.CallEffect) error {
	meta := agent.CallMeta(next.State)
	if hasEffect(effs, agent.EffLog) != (meta.LogStatus != "") {
		return fmt.Errorf("C2: %v -> %v effects %v, log status %q", before.State, next.State, effs, meta.LogStatus)
	}
	if meta.Terminal && !hasEffect(effs, agent.EffReply) {
		return fmt.Errorf("C4: terminal %v emits no reply: %v", next.State, effs)
	}
	if hasEffect(effs, agent.EffRemember) != (meta.Outcome != "") {
		return fmt.Errorf("C5: %v -> %v effects %v, outcome %q", before.State, next.State, effs, meta.Outcome)
	}
	return nil
}

func callMachine() machinetest.Machine[agent.CallState, agent.CallKind, agent.CallSnap, agent.CallKind] {
	return machinetest.Machine[agent.CallState, agent.CallKind, agent.CallSnap, agent.CallKind]{
		States:  agent.CallStates(),
		Kinds:   agent.CallKinds(),
		Initial: agent.CallSnap{State: agent.CallProposed, Impact: agent.ImpactHeavy},
		State:   func(s agent.CallSnap) agent.CallState { return s.State },
		Key:     func(s agent.CallSnap) string { return fmt.Sprintf("%+v", s) },
		Samples: callSamples,
		Events:  callEvents,
		Matches: agent.CallMatches,
		Next: func(s agent.CallSnap, k agent.CallKind) (agent.CallSnap, error) {
			n, _, err := agent.NextCall(s, k)
			return n, err
		},
		Check:    agent.CheckCall,
		Terminal: func(s agent.CallState) bool { return agent.CallMeta(s).Terminal },
		Extra:    callObligations,
	}
}

func TestCallMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, callMachine())
}

func TestCallMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, callMachine())
}

func TestCallMachine_Invariants(t *testing.T) {
	t.Parallel()
	m := callMachine()
	for _, s := range m.States {
		for _, snap := range callSamples(s) {
			if err := agent.CheckCall(snap); err != nil {
				t.Errorf("sample %+v fails its own check: %v", snap, err)
			}
		}
	}
	machinetest.Invariants(t, m)
}

func TestCallMachine_CheckRejectsBrokenSnapshots(t *testing.T) {
	t.Parallel()
	broken := []agent.CallSnap{
		{State: agent.CallActed},
		{State: agent.CallProposed, Counted: true},
		{State: agent.CallApplied, Impact: agent.ImpactShared, Counted: true},
		{State: agent.CallApplied, Impact: agent.ImpactItem, Counted: true, Disruptive: true},
		{State: agent.CallInterrupted, Impact: agent.ImpactHeavy, Counted: true, Disruptive: true},
		{State: "bogus"},
	}
	for _, s := range broken {
		if agent.CheckCall(s) == nil {
			t.Errorf("check accepted %+v", s)
		}
	}
}

func TestCallMeta_Outcomes(t *testing.T) {
	t.Parallel()
	want := map[string]agent.CallOutcome{
		string(agent.CallRefused):     agent.OutcomeRefused,
		string(agent.CallApplied):     agent.OutcomeApplied,
		string(agent.CallFailed):      agent.OutcomeFailed,
		string(agent.CallInterrupted): agent.OutcomeInterrupted,
	}
	for _, s := range agent.CallStates() {
		if got := agent.CallMeta(s).Outcome; got != want[string(s)] {
			t.Errorf("%v outcome = %q, want %q", s, got, want[string(s)])
		}
	}
	if len(agent.CallOutcomes()) != 5 || len(agent.CallKinds()) != 14 || len(agent.CallStates()) != 11 {
		t.Errorf(
			"sizes %d/%d/%d, want 5/14/11",
			len(agent.CallOutcomes()), len(agent.CallKinds()), len(agent.CallStates()),
		)
	}
}

func callByte(k agent.CallKind) byte { return byte(slices.Index(agent.CallKinds(), k)) }

func callPath(steps ...agent.CallKind) []byte {
	out := make([]byte, 0, len(steps))
	for _, k := range steps {
		out = append(out, callByte(k))
	}
	return out
}

func FuzzCallMachine(f *testing.F) {
	f.Add(callPath(agent.EvCleared, agent.EvGateAllowed, agent.EvSucceeded))
	f.Add(callPath(agent.EvCleared, agent.EvGateAllowed, agent.EvNotReady))
	f.Add(callPath(agent.EvCleared, agent.EvGateAllowed, agent.EvErrored))
	f.Add(callPath(agent.EvCleared, agent.EvGateAllowed, agent.EvCut))
	f.Add(callPath(agent.EvCleared, agent.EvRefusal))
	f.Add(callPath(agent.EvCleared, agent.EvGateDenied))
	f.Add(callPath(agent.EvLiveDenied, agent.EvCleared))
	for _, k := range agent.CallKinds() {
		f.Add(callPath(k))
		f.Add(callPath(agent.EvCleared, k))
		f.Add(callPath(agent.EvCleared, agent.EvGateAllowed, k))
	}
	m := callMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

type callRowCase struct {
	from    agent.CallState
	kinds   []agent.CallKind
	to      agent.CallState
	reject  string
	effects []agent.CallEffect
}

func callPlanRows() []callRowCase {
	reply := []agent.CallEffect{agent.EffReply}
	replyVerdict := []agent.CallEffect{agent.EffReply, agent.EffVerdict}
	logged := []agent.CallEffect{agent.EffLog, agent.EffRemember, agent.EffReply}
	screen := []agent.CallKind{
		agent.EvLiveDenied, agent.EvAppliedBefore, agent.EvUnknownBefore, agent.EvSeenInRun, agent.EvCleared,
	}
	acts := []agent.CallKind{
		agent.EvRefusal, agent.EvPreconditionFailed, agent.EvGateDenied, agent.EvGateFailed, agent.EvGateAllowed,
	}
	results := []agent.CallKind{agent.EvSucceeded, agent.EvNotReady, agent.EvErrored, agent.EvCut}
	one := func(k agent.CallKind) []agent.CallKind { return []agent.CallKind{k} }
	rows := []callRowCase{
		{agent.CallProposed, one(agent.EvLiveDenied), agent.CallNotRun, "", replyVerdict},
		{agent.CallProposed, one(agent.EvAppliedBefore), agent.CallBlockedApplied, "", reply},
		{agent.CallProposed, one(agent.EvUnknownBefore), agent.CallBlockedUnknown, "", reply},
		{agent.CallProposed, one(agent.EvSeenInRun), agent.CallBlockedRepeat, "", reply},
		{agent.CallProposed, one(agent.EvCleared), agent.CallScreened, "", nil},
		{agent.CallProposed, slices.Concat(acts, results), "", "not screened", nil},
		{agent.CallScreened, one(agent.EvRefusal), agent.CallRefused, "", logged},
		{agent.CallScreened, one(agent.EvPreconditionFailed), agent.CallNotRun, "", reply},
		{agent.CallScreened, one(agent.EvGateDenied), agent.CallNotRun, "", replyVerdict},
		{agent.CallScreened, one(agent.EvGateFailed), agent.CallNotRun, "", reply},
		{agent.CallScreened, one(agent.EvGateAllowed), agent.CallActed, "", nil},
		{agent.CallScreened, screen, "", "already screened", nil},
		{agent.CallScreened, results, "", "not acted", nil},
		{agent.CallActed, one(agent.EvSucceeded), agent.CallApplied, "", logged},
		{agent.CallActed, one(agent.EvNotReady), agent.CallApplied, "", logged},
		{agent.CallActed, one(agent.EvErrored), agent.CallFailed, "", logged},
		{
			agent.CallActed, one(agent.EvCut), agent.CallInterrupted, "",
			[]agent.CallEffect{agent.EffRemember, agent.EffReply},
		},
		{agent.CallActed, slices.Concat(screen, acts), "", "already acted", nil},
	}
	for _, s := range []agent.CallState{
		agent.CallBlockedApplied, agent.CallBlockedUnknown, agent.CallBlockedRepeat, agent.CallNotRun,
		agent.CallRefused, agent.CallApplied, agent.CallFailed, agent.CallInterrupted,
	} {
		rows = append(rows, callRowCase{s, agent.CallKinds(), "", "call ended", nil})
	}
	return rows
}

func TestCallMachine_RowsMatchPlan(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range callPlanRows() {
		for _, k := range c.kinds {
			key := fmt.Sprintf("%v/%v", c.from, k)
			if seen[key] {
				t.Errorf("%s listed twice", key)
			}
			seen[key] = true
			checkPlanRow(t, c, k)
		}
	}
	if want := len(agent.CallStates()) * len(agent.CallKinds()); len(seen) != want {
		t.Errorf("plan lists %d pairs, want %d", len(seen), want)
	}
}

func checkPlanRow(t *testing.T, c callRowCase, k agent.CallKind) {
	t.Helper()
	from := agent.CallSnap{State: c.from, Counted: agent.CallMeta(c.from).Counted}
	got, effs, err := agent.NextCall(from, k)
	if c.reject != "" {
		want := fmt.Sprintf("call %s rejects %s: %s", c.from, k, c.reject)
		if err == nil || err.Error() != want || got != from || effs != nil {
			t.Errorf("%v x %v = %+v %v %v, want reject %q", c.from, k, got, effs, err, want)
		}
		return
	}
	if err != nil {
		t.Errorf("%v x %v: %v", c.from, k, err)
		return
	}
	if got.State != c.to || !slices.Equal(effs, c.effects) {
		t.Errorf("%v x %v = %v %v, want %v %v", c.from, k, got.State, effs, c.to, c.effects)
	}
}

func TestCallMachine_CountedAndDisruptive(t *testing.T) {
	t.Parallel()
	for _, imp := range callImpacts() {
		snap := agent.CallSnap{State: agent.CallScreened, Impact: imp}
		acted, _, err := agent.NextCall(snap, agent.EvGateAllowed)
		if err != nil || !acted.Counted || acted.Disruptive {
			t.Fatalf("impact %v acted = %+v %v", imp, acted, err)
		}
		for _, k := range []agent.CallKind{agent.EvSucceeded, agent.EvNotReady, agent.EvErrored, agent.EvCut} {
			end, _, endErr := agent.NextCall(acted, k)
			want := imp >= agent.ImpactShared && k != agent.EvCut
			if endErr != nil || !end.Counted || end.Disruptive != want {
				t.Errorf("impact %v %v = %+v %v, want disruptive %v", imp, k, end, endErr, want)
			}
		}
	}
}
