package agent

import (
	"errors"
	"fmt"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
)

type RunState string

const (
	RunClaiming       RunState = "claiming"
	RunAwaitingReply  RunState = "awaiting_reply"
	RunInBatch        RunState = "in_batch"
	RunConcluded      RunState = "concluded"
	RunBudgetTripped  RunState = "budget_tripped"
	RunStoppedNoTools RunState = "stopped_no_tools"
	RunExhausted      RunState = "exhausted"
	RunLLMFailed      RunState = "llm_failed"
	RunBatchFailed    RunState = "batch_failed"
	RunSuperseded     RunState = "superseded"
	RunNotClaimed     RunState = "not_claimed"
	RunClaimFailed    RunState = "claim_failed"
	RunPaused         RunState = "paused"
)

type RunStateMeta struct {
	Terminal      bool
	WritesFinding bool
	Label         string
	Reason        string
	NotRun        string
}

type RunExit struct {
	State        RunState
	Rounds       int
	Finding      *DiagnosticResult
	Conversation []openai.ChatCompletionMessage
	Err          error
	Started      bool
	Journal      *db.Event
}

func RunMeta(s RunState) RunStateMeta {
	return runMetas()[s]
}

func runStates() []RunState {
	return []RunState{
		RunClaiming, RunAwaitingReply, RunInBatch, RunConcluded, RunBudgetTripped, RunStoppedNoTools,
		RunExhausted, RunLLMFailed, RunBatchFailed, RunSuperseded, RunNotClaimed, RunClaimFailed, RunPaused,
	}
}

func runMetas() map[RunState]RunStateMeta {
	return map[RunState]RunStateMeta{
		RunClaiming:      {Label: "claiming"},
		RunAwaitingReply: {Label: "awaiting reply", NotRun: "not answered"},
		RunInBatch:       {Label: "running tools"},
		RunConcluded:     {Terminal: true, Label: "concluded", NotRun: "complete_diagnosis ended the run"},
		RunBudgetTripped: {
			Terminal: true, WritesFinding: true, Label: "action limit reached",
			Reason: "max autonomous actions reached without resolution", NotRun: "autonomous action limit reached",
		},
		RunStoppedNoTools: {
			Terminal: true, WritesFinding: true, Label: "stopped without a diagnosis",
			Reason: "the agent stopped without calling complete_diagnosis",
		},
		RunExhausted: {
			Terminal: true, WritesFinding: true, Label: "round limit reached",
			Reason: "diagnostic loop exhausted without conclusion", NotRun: "round limit reached",
		},
		RunLLMFailed: {Terminal: true, WritesFinding: true, Label: "LLM failed", Reason: "the LLM call failed"},
		RunBatchFailed: {
			Terminal: true, WritesFinding: true, Label: "diagnosis not recorded",
			Reason: "the diagnosis could not be recorded", NotRun: "the run failed",
		},
		RunSuperseded: {Terminal: true, Label: "stopped", NotRun: "the run was stopped"},
		RunNotClaimed: {Terminal: true, Label: "not claimed"},
		RunClaimFailed: {
			Terminal: true, WritesFinding: true, Label: "claim failed", Reason: "the run could not claim the incident",
		},
		RunPaused: {Terminal: true, Label: "paused", NotRun: "autonomous actions paused"},
	}
}

type runEventKind string

const (
	evClaimed       runEventKind = "claimed"
	evClaimRefused  runEventKind = "claim_refused"
	evClaimFailed   runEventKind = "claim_failed"
	evToolCalls     runEventKind = "tool_calls"
	evTextOnly      runEventKind = "text_only"
	evLLMFailed     runEventKind = "llm_failed"
	evConcluded     runEventKind = "concluded"
	evBudgetTripped runEventKind = "budget_tripped"
	evBatchFailed   runEventKind = "batch_failed"
	evBatchDone     runEventKind = "batch_done"
	evPaused        runEventKind = "paused"
	evStale         runEventKind = "stale"
)

func runEventKinds() []runEventKind {
	return []runEventKind{
		evClaimed, evClaimRefused, evClaimFailed, evToolCalls, evTextOnly,
		evLLMFailed, evConcluded, evBudgetTripped, evBatchFailed, evBatchDone, evPaused, evStale,
	}
}

func claimKinds() []runEventKind { return []runEventKind{evClaimed, evClaimRefused, evClaimFailed} }

func replyKinds() []runEventKind { return []runEventKind{evToolCalls, evTextOnly, evLLMFailed} }

func batchKinds() []runEventKind {
	return []runEventKind{evConcluded, evBudgetTripped, evBatchFailed, evBatchDone}
}

func gateKinds() []runEventKind { return []runEventKind{evPaused, evStale} }

type runEvent struct {
	kind      runEventKind
	cancelled bool
	err       error
}

type runSnapshot struct {
	State   RunState
	Round   int
	Replies int
	Started bool
}

type runEffect string

const (
	effRecordStarted runEffect = "record_started"
	effAnswerPending runEffect = "answer_pending"
	effRecordRound   runEffect = "record_round"
	effFinish        runEffect = "finish"
)

type runGuard string

const (
	guardAny       runGuard = "any"
	guardLive      runGuard = "live"
	guardCancelled runGuard = "cancelled"
	guardMore      runGuard = "live, rounds left"
	guardLast      runGuard = "live, last round"
)

func (g runGuard) holds(s runSnapshot, ev runEvent) bool {
	switch g {
	case guardAny:
		return true
	case guardLive:
		return !ev.cancelled
	case guardCancelled:
		return ev.cancelled
	case guardMore:
		return !ev.cancelled && s.Round+1 < maxRounds
	case guardLast:
		return !ev.cancelled && s.Round+1 >= maxRounds
	}
	return false
}

type runRow struct {
	from    RunState
	kind    runEventKind
	guard   runGuard
	to      RunState
	reject  string
	effects []runEffect
}

func runAllowedRows() []runRow {
	ap := []runEffect{effAnswerPending, effRecordRound}
	apf := []runEffect{effAnswerPending, effRecordRound, effFinish}
	fin := []runEffect{effFinish}
	rrf := []runEffect{effRecordRound, effFinish}
	return []runRow{
		{RunClaiming, evClaimed, guardAny, RunAwaitingReply, "", []runEffect{effRecordStarted}},
		{RunClaiming, evClaimRefused, guardAny, RunNotClaimed, "", fin},
		{RunClaiming, evClaimFailed, guardLive, RunClaimFailed, "", fin},
		{RunClaiming, evClaimFailed, guardCancelled, RunSuperseded, "", fin},
		{RunAwaitingReply, evToolCalls, guardAny, RunInBatch, "", nil},
		{RunAwaitingReply, evTextOnly, guardLive, RunStoppedNoTools, "", rrf},
		{RunAwaitingReply, evTextOnly, guardCancelled, RunSuperseded, "", rrf},
		{RunAwaitingReply, evLLMFailed, guardLive, RunLLMFailed, "", fin},
		{RunAwaitingReply, evLLMFailed, guardCancelled, RunSuperseded, "", fin},
		{RunInBatch, evConcluded, guardAny, RunConcluded, "", apf},
		{RunInBatch, evBudgetTripped, guardLive, RunBudgetTripped, "", apf},
		{RunInBatch, evBudgetTripped, guardCancelled, RunSuperseded, "", apf},
		{RunInBatch, evBatchFailed, guardLive, RunBatchFailed, "", apf},
		{RunInBatch, evBatchFailed, guardCancelled, RunSuperseded, "", apf},
		{RunInBatch, evBatchDone, guardMore, RunAwaitingReply, "", ap},
		{RunInBatch, evBatchDone, guardLast, RunExhausted, "", apf},
		{RunInBatch, evBatchDone, guardCancelled, RunSuperseded, "", apf},
		{RunAwaitingReply, evPaused, guardAny, RunPaused, "", fin},
		{RunAwaitingReply, evStale, guardAny, RunSuperseded, "", fin},
		{RunInBatch, evPaused, guardAny, RunPaused, "", apf},
		{RunInBatch, evStale, guardAny, RunSuperseded, "", apf},
	}
}

func rejectAll(from RunState, kinds []runEventKind, reason string) []runRow {
	rows := make([]runRow, 0, len(kinds))
	for _, k := range kinds {
		rows = append(rows, runRow{from: from, kind: k, guard: guardAny, reject: reason})
	}
	return rows
}

func runRejectRows() []runRow {
	var rows []runRow
	rows = append(rows, rejectAll(RunClaiming, replyKinds(), "the run has not started")...)
	rows = append(rows, rejectAll(RunClaiming, batchKinds(), "the run has not started")...)
	rows = append(rows, rejectAll(RunClaiming, gateKinds(), "the run has not started")...)
	rows = append(rows, rejectAll(RunAwaitingReply, claimKinds(), "no batch is open")...)
	rows = append(rows, rejectAll(RunAwaitingReply, batchKinds(), "no batch is open")...)
	rows = append(rows, rejectAll(RunInBatch, claimKinds(), "a batch is open")...)
	rows = append(rows, rejectAll(RunInBatch, replyKinds(), "a batch is open")...)
	for _, s := range runStates() {
		if RunMeta(s).Terminal {
			rows = append(rows, rejectAll(s, runEventKinds(), "the run already ended")...)
		}
	}
	return rows
}

func runRows() []runRow {
	return append(runAllowedRows(), runRejectRows()...)
}

func runMatches(s runSnapshot, ev runEvent) int {
	n := 0
	for _, r := range runRows() {
		if r.from == s.State && r.kind == ev.kind && r.guard.holds(s, ev) {
			n++
		}
	}
	return n
}

func nextRun(cur runSnapshot, ev runEvent) (runSnapshot, []runEffect, error) {
	for _, r := range runRows() {
		if r.from != cur.State || r.kind != ev.kind || !r.guard.holds(cur, ev) {
			continue
		}
		if r.reject != "" {
			return cur, nil, fmt.Errorf("run %s rejects %s: %s", cur.State, ev.kind, r.reject)
		}
		next := cur
		next.State = r.to
		if ev.kind == evClaimed {
			next.Started = true
		}
		if ev.kind == evToolCalls || ev.kind == evTextOnly {
			next.Replies++
		}
		if cur.State == RunInBatch && r.to == RunAwaitingReply {
			next.Round++
		}
		return next, append([]runEffect(nil), r.effects...), nil
	}
	return cur, nil, fmt.Errorf("run %s has no row for %s", cur.State, ev.kind)
}

func failState(cur runSnapshot, ev runEvent) RunState {
	if RunMeta(cur.State).Terminal {
		return cur.State
	}
	if ev.cancelled || ev.kind == evStale {
		return RunSuperseded
	}
	if ev.kind == evPaused && cur.State != RunClaiming {
		return RunPaused
	}
	switch cur.State {
	case RunClaiming:
		return RunClaimFailed
	case RunAwaitingReply:
		return RunLLMFailed
	case RunInBatch:
		return RunBatchFailed
	case RunConcluded, RunBudgetTripped, RunStoppedNoTools, RunExhausted, RunLLMFailed,
		RunBatchFailed, RunSuperseded, RunNotClaimed, RunClaimFailed, RunPaused:
	}
	return cur.State
}

func failRun(cur runSnapshot, ev runEvent) (runSnapshot, []runEffect) {
	to := failState(cur, ev)
	if to == cur.State {
		return cur, nil
	}
	next := cur
	next.State = to
	if cur.State == RunInBatch {
		return next, []runEffect{effAnswerPending, effRecordRound, effFinish}
	}
	return next, []runEffect{effFinish}
}

var errBadRunCounters = errors.New("run counters out of range")

func checkRun(s runSnapshot) error {
	if _, ok := runMetas()[s.State]; !ok {
		return fmt.Errorf("unknown run state %q", s.State)
	}
	if s.Round < 0 || s.Round > s.Replies || s.Replies > maxRounds {
		return fmt.Errorf("R8: %w: %+v", errBadRunCounters, s)
	}
	if err := checkRunState(s); err != nil {
		return fmt.Errorf("%w: %+v", err, s)
	}
	return nil
}

func checkRunState(s runSnapshot) error {
	switch s.State {
	case RunClaiming:
		if s.Started || s.Replies != 0 {
			return errors.New("R1: claiming has started")
		}
	case RunAwaitingReply:
		if !s.Started || s.Replies != s.Round || s.Round >= maxRounds {
			return errors.New("R2: awaiting reply out of step")
		}
	case RunInBatch:
		if !s.Started || s.Replies != s.Round+1 {
			return errors.New("R3: batch out of step")
		}
	case RunNotClaimed, RunClaimFailed:
		if s.Started || s.Replies != 0 {
			return errors.New("R4: an unclaimed run has started")
		}
	case RunExhausted:
		if s.Replies != maxRounds {
			return errors.New("R5: exhausted before the round limit")
		}
	case RunStoppedNoTools, RunConcluded, RunBudgetTripped, RunBatchFailed:
		if !s.Started || s.Replies < 1 {
			return errors.New("R6: ended a batch without a reply")
		}
	case RunLLMFailed, RunPaused:
		return requireStarted(s)
	case RunSuperseded:
	}
	return nil
}

func requireStarted(s runSnapshot) error {
	switch {
	case s.Started:
		return nil
	case s.State == RunPaused:
		return errors.New("R9: paused before the run started")
	}
	return errors.New("R7: LLM failed before the run started")
}
