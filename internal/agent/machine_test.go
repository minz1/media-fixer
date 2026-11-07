package agent_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/machinetest"
)

const maxR = agent.MaxRoundsForTest

func runSamples(s agent.RunState) []agent.RunSnapshot {
	switch s {
	case agent.RunClaiming, agent.RunNotClaimed, agent.RunClaimFailed:
		return []agent.RunSnapshot{{State: s}}
	case agent.RunAwaitingReply:
		return []agent.RunSnapshot{
			{State: s, Started: true},
			{State: s, Started: true, Round: 7, Replies: 7},
			{State: s, Started: true, Round: maxR - 1, Replies: maxR - 1},
		}
	case agent.RunInBatch:
		return []agent.RunSnapshot{
			{State: s, Started: true, Replies: 1},
			{State: s, Started: true, Round: maxR - 2, Replies: maxR - 1},
			{State: s, Started: true, Round: maxR - 1, Replies: maxR},
		}
	case agent.RunExhausted:
		return []agent.RunSnapshot{{State: s, Started: true, Round: maxR - 1, Replies: maxR}}
	case agent.RunLLMFailed:
		return []agent.RunSnapshot{{State: s, Started: true}, {State: s, Started: true, Round: 3, Replies: 3}}
	case agent.RunSuperseded:
		return []agent.RunSnapshot{{State: s}, {State: s, Started: true, Round: 2, Replies: 3}}
	case agent.RunPaused:
		return []agent.RunSnapshot{{State: s, Started: true}, {State: s, Started: true, Round: 2, Replies: 3}}
	case agent.RunConcluded, agent.RunBudgetTripped, agent.RunStoppedNoTools, agent.RunBatchFailed:
		return []agent.RunSnapshot{
			{State: s, Started: true, Replies: 1},
			{State: s, Started: true, Round: 4, Replies: 5},
		}
	}
	return nil
}

func runEvents(k agent.RunEventKind) []agent.RunEvent {
	return []agent.RunEvent{agent.NewRunEvent(k, false), agent.NewRunEvent(k, true)}
}

func countEff(effs []agent.RunEffect, want agent.RunEffect) int {
	n := 0
	for _, e := range effs {
		if e == want {
			n++
		}
	}
	return n
}

func runObligations(before agent.RunSnapshot, ev agent.RunEvent) error {
	next, effs, err := agent.NextRun(before, ev)
	if err == nil {
		return effectObligations(before, ev, next, effs)
	}
	return nil
}

func effectObligations(
	before agent.RunSnapshot,
	ev agent.RunEvent,
	next agent.RunSnapshot,
	effs []agent.RunEffect,
) error {
	meta := agent.RunMeta(next.State)
	finishes := countEff(effs, agent.EffFinish)
	if meta.Terminal && (finishes != 1 || effs[len(effs)-1] != agent.EffFinish) {
		return fmt.Errorf("E1: %v -> %v effects %v, want one trailing finish", before.State, next.State, effs)
	}
	if !meta.Terminal && finishes != 0 {
		return fmt.Errorf("E1: non-terminal %v emits finish", next.State)
	}
	if before.State == agent.RunInBatch &&
		(len(effs) < 2 || effs[0] != agent.EffAnswerPending || effs[1] != agent.EffRecordRound) {
		return fmt.Errorf("E2: leaving in_batch with %v, want answer pending then record round first", effs)
	}
	started := before.State == agent.RunClaiming && next.State == agent.RunAwaitingReply
	if (countEff(effs, agent.EffRecordStarted) == 1) != started || countEff(effs, agent.EffRecordStarted) > 1 {
		return fmt.Errorf("E3: %v -> %v effects %v", before.State, next.State, effs)
	}
	if countEff(effs, agent.EffRecordRound) > 0 && !before.Started {
		return errors.New("E4: record round before the run started")
	}
	if ev.Cancelled() && meta.WritesFinding {
		return fmt.Errorf("E5: cancelled %v lands in %v, which writes a finding", ev.Kind(), next.State)
	}
	if (ev.Kind() == agent.EvPaused || ev.Kind() == agent.EvStale) && meta.WritesFinding {
		return fmt.Errorf("E5: %v lands in %v, which writes a finding", ev.Kind(), next.State)
	}
	return nil
}

func runMachine() machinetest.Machine[agent.RunState, agent.RunEventKind, agent.RunSnapshot, agent.RunEvent] {
	return machinetest.Machine[agent.RunState, agent.RunEventKind, agent.RunSnapshot, agent.RunEvent]{
		States:  agent.RunStates(),
		Kinds:   agent.RunEventKinds(),
		Initial: agent.RunSnapshot{State: agent.RunClaiming},
		State:   func(s agent.RunSnapshot) agent.RunState { return s.State },
		Key:     func(s agent.RunSnapshot) string { return fmt.Sprintf("%+v", s) },
		Samples: runSamples,
		Events:  runEvents,
		Matches: agent.RunMatches,
		Next: func(s agent.RunSnapshot, ev agent.RunEvent) (agent.RunSnapshot, error) {
			n, _, err := agent.NextRun(s, ev)
			return n, err
		},
		Check:    agent.CheckRun,
		Terminal: func(s agent.RunState) bool { return agent.RunMeta(s).Terminal },
		Extra:    runObligations,
	}
}

func TestRunMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, runMachine())
}

func TestRunMachine_ReachabilityAndExits(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, runMachine())
}

func TestRunMachine_Invariants(t *testing.T) {
	t.Parallel()
	m := runMachine()
	for _, s := range m.States {
		for _, snap := range runSamples(s) {
			if err := agent.CheckRun(snap); err != nil {
				t.Errorf("sample %+v fails its own check: %v", snap, err)
			}
		}
	}
	machinetest.Invariants(t, m)
}

func TestRunMachine_CheckRejectsBrokenSnapshots(t *testing.T) {
	t.Parallel()
	broken := []agent.RunSnapshot{
		{State: agent.RunClaiming, Started: true},
		{State: agent.RunAwaitingReply, Started: true, Round: 1, Replies: 2},
		{State: agent.RunAwaitingReply, Started: true, Round: maxR, Replies: maxR},
		{State: agent.RunInBatch, Started: true, Round: 1, Replies: 1},
		{State: agent.RunNotClaimed, Started: true},
		{State: agent.RunExhausted, Started: true, Round: 3, Replies: 4},
		{State: agent.RunStoppedNoTools, Started: true},
		{State: agent.RunLLMFailed},
		{State: agent.RunSuperseded, Round: 3, Replies: 2},
		{State: "bogus"},
	}
	for _, s := range broken {
		if agent.CheckRun(s) == nil {
			t.Errorf("check accepted %+v", s)
		}
	}
}

func evByte(k agent.RunEventKind, cancelled bool) byte {
	i := slices.Index(agent.RunEventKinds(), k) * 2
	if cancelled {
		i++
	}
	return byte(i)
}

func path(steps ...agent.RunEventKind) []byte {
	out := make([]byte, 0, len(steps))
	for _, k := range steps {
		out = append(out, evByte(k, false))
	}
	return out
}

func longPath() []byte {
	steps := []agent.RunEventKind{agent.EvClaimed}
	for range maxR {
		steps = append(steps, agent.EvToolCalls, agent.EvBatchDone)
	}
	return path(steps...)
}

func FuzzRunMachine(f *testing.F) {
	f.Add(path(agent.EvClaimed, agent.EvToolCalls, agent.EvConcluded))
	f.Add(path(agent.EvClaimRefused))
	f.Add(path(agent.EvClaimFailed))
	f.Add(path(agent.EvClaimed, agent.EvTextOnly))
	f.Add(path(agent.EvClaimed, agent.EvLLMFailed))
	f.Add(path(agent.EvClaimed, agent.EvToolCalls, agent.EvBudgetTripped))
	f.Add(path(agent.EvClaimed, agent.EvToolCalls, agent.EvBatchFailed))
	f.Add(path(agent.EvClaimed, agent.EvToolCalls, agent.EvBatchDone, agent.EvToolCalls, agent.EvConcluded))
	f.Add(longPath())
	for _, k := range agent.RunEventKinds() {
		f.Add([]byte{evByte(agent.EvClaimed, false), evByte(agent.EvToolCalls, false), evByte(k, true)})
		f.Add([]byte{evByte(agent.EvClaimed, false), evByte(k, true)})
		f.Add([]byte{evByte(k, true)})
	}
	m := runMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

type rowCase struct {
	from      agent.RunSnapshot
	kind      agent.RunEventKind
	cancelled bool
	to        agent.RunSnapshot
	effects   []agent.RunEffect
}

func batch(round int) agent.RunSnapshot {
	return agent.RunSnapshot{State: agent.RunInBatch, Started: true, Round: round, Replies: round + 1}
}

func ended(s agent.RunState, round, replies int) agent.RunSnapshot {
	return agent.RunSnapshot{State: s, Started: true, Round: round, Replies: replies}
}

func rowCases() []rowCase {
	claiming := agent.RunSnapshot{State: agent.RunClaiming}
	waiting := agent.RunSnapshot{State: agent.RunAwaitingReply, Started: true, Round: 2, Replies: 2}
	ap := []agent.RunEffect{agent.EffAnswerPending, agent.EffRecordRound}
	apf := []agent.RunEffect{agent.EffAnswerPending, agent.EffRecordRound, agent.EffFinish}
	fin := []agent.RunEffect{agent.EffFinish}
	rrf := []agent.RunEffect{agent.EffRecordRound, agent.EffFinish}
	return []rowCase{
		{
			claiming,
			agent.EvClaimed,
			false,
			ended(agent.RunAwaitingReply, 0, 0),
			[]agent.RunEffect{agent.EffRecordStarted},
		},
		{claiming, agent.EvClaimRefused, false, agent.RunSnapshot{State: agent.RunNotClaimed}, fin},
		{claiming, agent.EvClaimFailed, false, agent.RunSnapshot{State: agent.RunClaimFailed}, fin},
		{claiming, agent.EvClaimFailed, true, agent.RunSnapshot{State: agent.RunSuperseded}, fin},
		{waiting, agent.EvToolCalls, false, batch(2), nil},
		{waiting, agent.EvTextOnly, false, ended(agent.RunStoppedNoTools, 2, 3), rrf},
		{waiting, agent.EvTextOnly, true, ended(agent.RunSuperseded, 2, 3), rrf},
		{waiting, agent.EvLLMFailed, false, ended(agent.RunLLMFailed, 2, 2), fin},
		{waiting, agent.EvLLMFailed, true, ended(agent.RunSuperseded, 2, 2), fin},
		{batch(2), agent.EvConcluded, false, ended(agent.RunConcluded, 2, 3), apf},
		{batch(2), agent.EvBudgetTripped, false, ended(agent.RunBudgetTripped, 2, 3), apf},
		{batch(2), agent.EvBudgetTripped, true, ended(agent.RunSuperseded, 2, 3), apf},
		{batch(2), agent.EvBatchFailed, false, ended(agent.RunBatchFailed, 2, 3), apf},
		{batch(2), agent.EvBatchFailed, true, ended(agent.RunSuperseded, 2, 3), apf},
		{batch(2), agent.EvBatchDone, false, ended(agent.RunAwaitingReply, 3, 3), ap},
		{batch(maxR - 1), agent.EvBatchDone, false, ended(agent.RunExhausted, maxR-1, maxR), apf},
		{batch(2), agent.EvBatchDone, true, ended(agent.RunSuperseded, 2, 3), apf},
		{waiting, agent.EvPaused, false, ended(agent.RunPaused, 2, 2), fin},
		{waiting, agent.EvStale, false, ended(agent.RunSuperseded, 2, 2), fin},
		{batch(2), agent.EvPaused, false, ended(agent.RunPaused, 2, 3), apf},
		{batch(2), agent.EvStale, false, ended(agent.RunSuperseded, 2, 3), apf},
	}
}

func TestRunMachine_TableRows(t *testing.T) {
	t.Parallel()
	cases := rowCases()
	allowed, rejects := agent.RunRowCountsForTest()
	if len(cases) != allowed {
		t.Fatalf("%d cases for %d allowed rows", len(cases), allowed)
	}
	if rejects == 0 {
		t.Fatal("no reject rows")
	}
	for _, reason := range agent.RejectReasonsForTest() {
		if reason == "" {
			t.Error("a reject row has no reason")
		}
	}
	for _, c := range cases {
		got, effs, err := agent.NextRun(c.from, agent.NewRunEvent(c.kind, c.cancelled))
		if err != nil {
			t.Errorf("%v x %v (c=%v): %v", c.from.State, c.kind, c.cancelled, err)
			continue
		}
		if got != c.to || !slices.Equal(effs, c.effects) {
			t.Errorf(
				"%v x %v (c=%v) = %+v %v, want %+v %v",
				c.from.State,
				c.kind,
				c.cancelled,
				got,
				effs,
				c.to,
				c.effects,
			)
		}
	}
}

func TestRunMachine_RejectLeavesSnapshot(t *testing.T) {
	t.Parallel()
	cur := agent.RunSnapshot{State: agent.RunAwaitingReply, Started: true, Round: 1, Replies: 1}
	got, effs, err := agent.NextRun(cur, agent.NewRunEvent(agent.EvBatchDone, false))
	if err == nil || got != cur || effs != nil {
		t.Fatalf("batch done while awaiting a reply = %+v %v %v, want a reject that changes nothing", got, effs, err)
	}
}

func fallbackTarget(s agent.RunState, cancelled bool) agent.RunState {
	if agent.RunMeta(s).Terminal {
		return s
	}
	if cancelled {
		return agent.RunSuperseded
	}
	pairs := [][2]agent.RunState{
		{agent.RunClaiming, agent.RunClaimFailed},
		{agent.RunAwaitingReply, agent.RunLLMFailed},
		{agent.RunInBatch, agent.RunBatchFailed},
	}
	for _, p := range pairs {
		if p[0] == s {
			return p[1]
		}
	}
	return s
}

func checkFallback(t *testing.T, snap agent.RunSnapshot, cancelled bool) {
	t.Helper()
	ev := agent.NewRunEvent(agent.EvClaimed, cancelled)
	got, effs := agent.FailRun(snap, ev)
	if err := agent.CheckRun(got); err != nil {
		t.Errorf("fallback from %+v (c=%v) = %+v fails check: %v", snap, cancelled, got, err)
	}
	if want := fallbackTarget(snap.State, cancelled); got.State != want {
		t.Errorf("fallback from %v (c=%v) = %v, want %v", snap.State, cancelled, got.State, want)
	}
	if agent.RunMeta(snap.State).Terminal {
		if got != snap || effs != nil {
			t.Errorf("fallback from terminal %+v moved to %+v %v", snap, got, effs)
		}
		return
	}
	if err := effectObligations(snap, ev, got, effs); err != nil {
		t.Errorf("fallback from %v (c=%v): %v", snap.State, cancelled, err)
	}
}

func TestRunMachine_FailStateSatisfiesCheck(t *testing.T) {
	t.Parallel()
	for _, s := range agent.RunStates() {
		for _, snap := range runSamples(s) {
			checkFallback(t, snap, false)
			checkFallback(t, snap, true)
		}
	}
}

func TestRunMeta_EveryStateHasMeta(t *testing.T) {
	t.Parallel()
	live := []agent.RunState{agent.RunClaiming, agent.RunAwaitingReply, agent.RunInBatch}
	writes := []agent.RunState{
		agent.RunBudgetTripped, agent.RunStoppedNoTools, agent.RunExhausted,
		agent.RunLLMFailed, agent.RunBatchFailed, agent.RunClaimFailed,
	}
	if len(agent.RunStates()) != 13 {
		t.Fatalf("%d states, want 13", len(agent.RunStates()))
	}
	for _, s := range agent.RunStates() {
		m := agent.RunMeta(s)
		if m.Label == "" {
			t.Errorf("%v has no label", s)
		}
		if m.Terminal == slices.Contains(live, s) {
			t.Errorf("%v terminal = %v", s, m.Terminal)
		}
		if m.WritesFinding != slices.Contains(writes, s) {
			t.Errorf("%v writes finding = %v", s, m.WritesFinding)
		}
		if m.WritesFinding && m.Reason == "" {
			t.Errorf("%v writes a finding with no reason", s)
		}
	}
	for _, s := range []agent.RunState{
		agent.RunConcluded, agent.RunBudgetTripped, agent.RunBatchFailed,
		agent.RunSuperseded, agent.RunExhausted, agent.RunAwaitingReply,
	} {
		if agent.RunMeta(s).NotRun == "" {
			t.Errorf("%v has no not-run reason", s)
		}
	}
}
