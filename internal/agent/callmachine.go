package agent

import (
	"errors"
	"fmt"
)

type callState string

const (
	callProposed       callState = "proposed"
	callScreened       callState = "screened"
	callActed          callState = "acted"
	callBlockedApplied callState = "blocked_applied"
	callBlockedUnknown callState = "blocked_unknown"
	callBlockedRepeat  callState = "blocked_repeat"
	callNotRun         callState = "not_run"
	callRefused        callState = "refused"
	callApplied        callState = "applied"
	callFailed         callState = "failed"
	callInterrupted    callState = "interrupted"
)

func callStates() []callState {
	return []callState{
		callProposed, callScreened, callActed, callBlockedApplied, callBlockedUnknown, callBlockedRepeat,
		callNotRun, callRefused, callApplied, callFailed, callInterrupted,
	}
}

type callOutcome string

const (
	outcomeApplied     callOutcome = "applied"
	outcomeFailed      callOutcome = "failed"
	outcomeRefused     callOutcome = "refused"
	outcomeInterrupted callOutcome = "interrupted"
	outcomeInvalid     callOutcome = "invalid"
)

func callOutcomes() []callOutcome {
	return []callOutcome{outcomeApplied, outcomeFailed, outcomeRefused, outcomeInterrupted, outcomeInvalid}
}

type callStateMeta struct {
	Terminal  bool
	LogStatus string
	Outcome   callOutcome
	Counted   bool
}

func callMeta(s callState) callStateMeta {
	return callMetas()[s]
}

func callMetas() map[callState]callStateMeta {
	return map[callState]callStateMeta{
		callProposed:       {},
		callScreened:       {},
		callActed:          {Counted: true},
		callBlockedApplied: {Terminal: true},
		callBlockedUnknown: {Terminal: true},
		callBlockedRepeat:  {Terminal: true},
		callNotRun:         {Terminal: true},
		callRefused:        {Terminal: true, LogStatus: "refused", Outcome: outcomeRefused},
		callApplied:        {Terminal: true, LogStatus: "applied", Outcome: outcomeApplied, Counted: true},
		callFailed:         {Terminal: true, LogStatus: "failed", Outcome: outcomeFailed, Counted: true},
		callInterrupted:    {Terminal: true, Outcome: outcomeInterrupted, Counted: true},
	}
}

type callKind string

const (
	evLiveDenied         callKind = "live_denied"
	evAppliedBefore      callKind = "applied_before"
	evUnknownBefore      callKind = "unknown_before"
	evSeenInRun          callKind = "seen_in_run"
	evCleared            callKind = "cleared"
	evRefusal            callKind = "refusal"
	evPreconditionFailed callKind = "precondition_failed"
	evGateDenied         callKind = "gate_denied"
	evGateFailed         callKind = "gate_failed"
	evGateAllowed        callKind = "gate_allowed"
	evSucceeded          callKind = "succeeded"
	evNotReady           callKind = "not_ready"
	evErrored            callKind = "errored"
	evCut                callKind = "cut"
)

func callKinds() []callKind {
	return []callKind{
		evLiveDenied, evAppliedBefore, evUnknownBefore, evSeenInRun, evCleared, evRefusal, evPreconditionFailed,
		evGateDenied, evGateFailed, evGateAllowed, evSucceeded, evNotReady, evErrored, evCut,
	}
}

func screenKinds() []callKind {
	return []callKind{evLiveDenied, evAppliedBefore, evUnknownBefore, evSeenInRun, evCleared}
}

func actKinds() []callKind {
	return []callKind{evGateDenied, evGateFailed, evGateAllowed, evRefusal, evPreconditionFailed}
}

func resultKinds() []callKind {
	return []callKind{evSucceeded, evNotReady, evErrored, evCut}
}

type callEffect string

const (
	effReply    callEffect = "reply"
	effLog      callEffect = "log"
	effRemember callEffect = "remember"
	effVerdict  callEffect = "verdict"
)

type callSnap struct {
	State      callState
	Impact     toolImpact
	Counted    bool
	Disruptive bool
}

type callRow struct {
	from    callState
	kind    callKind
	to      callState
	reject  string
	effects []callEffect
}

func callAllowedRows() []callRow {
	reply := []callEffect{effReply}
	replyVerdict := []callEffect{effReply, effVerdict}
	logged := []callEffect{effLog, effRemember, effReply}
	return []callRow{
		{callProposed, evLiveDenied, callNotRun, "", replyVerdict},
		{callProposed, evAppliedBefore, callBlockedApplied, "", reply},
		{callProposed, evUnknownBefore, callBlockedUnknown, "", reply},
		{callProposed, evSeenInRun, callBlockedRepeat, "", reply},
		{callProposed, evCleared, callScreened, "", nil},
		{callScreened, evRefusal, callRefused, "", logged},
		{callScreened, evPreconditionFailed, callNotRun, "", reply},
		{callScreened, evGateDenied, callNotRun, "", replyVerdict},
		{callScreened, evGateFailed, callNotRun, "", reply},
		{callScreened, evGateAllowed, callActed, "", nil},
		{callActed, evSucceeded, callApplied, "", logged},
		{callActed, evNotReady, callApplied, "", logged},
		{callActed, evErrored, callFailed, "", logged},
		{callActed, evCut, callInterrupted, "", []callEffect{effRemember, effReply}},
	}
}

func callRejectAll(from callState, kinds []callKind, reason string) []callRow {
	rows := make([]callRow, 0, len(kinds))
	for _, k := range kinds {
		rows = append(rows, callRow{from: from, kind: k, reject: reason})
	}
	return rows
}

func callRejectRows() []callRow {
	var rows []callRow
	rows = append(rows, callRejectAll(callProposed, actKinds(), "not screened")...)
	rows = append(rows, callRejectAll(callProposed, resultKinds(), "not screened")...)
	rows = append(rows, callRejectAll(callScreened, screenKinds(), "already screened")...)
	rows = append(rows, callRejectAll(callScreened, resultKinds(), "not acted")...)
	rows = append(rows, callRejectAll(callActed, screenKinds(), "already acted")...)
	rows = append(rows, callRejectAll(callActed, actKinds(), "already acted")...)
	for _, s := range callStates() {
		if callMeta(s).Terminal {
			rows = append(rows, callRejectAll(s, callKinds(), "call ended")...)
		}
	}
	return rows
}

func callRows() []callRow {
	return append(callAllowedRows(), callRejectRows()...)
}

func callMatches(s callSnap, kind callKind) int {
	n := 0
	for _, r := range callRows() {
		if r.from == s.State && r.kind == kind {
			n++
		}
	}
	return n
}

func nextCall(cur callSnap, kind callKind) (callSnap, []callEffect, error) {
	for _, r := range callRows() {
		if r.from != cur.State || r.kind != kind {
			continue
		}
		if r.reject != "" {
			return cur, nil, fmt.Errorf("call %s rejects %s: %s", cur.State, kind, r.reject)
		}
		next := cur
		next.State = r.to
		next.Counted = callMeta(r.to).Counted
		next.Disruptive = cur.Impact >= impactShared && (r.to == callApplied || r.to == callFailed)
		return next, append([]callEffect(nil), r.effects...), nil
	}
	return cur, nil, fmt.Errorf("call %s has no row for %s", cur.State, kind)
}

func checkCall(s callSnap) error {
	meta, ok := callMetas()[s.State]
	if !ok {
		return fmt.Errorf("unknown call state %q", s.State)
	}
	if s.Counted != meta.Counted {
		return fmt.Errorf("C1: %w: %+v", errCallCounted, s)
	}
	want := s.Impact >= impactShared && (s.State == callApplied || s.State == callFailed)
	if s.Disruptive != want {
		return fmt.Errorf("C3: %w: %+v", errCallDisruptive, s)
	}
	return nil
}

var (
	errCallCounted    = errors.New("counted does not match the state")
	errCallDisruptive = errors.New("disruptive does not match the impact and state")
)
