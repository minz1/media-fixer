package lifecycle

import (
	"fmt"
	"slices"
	"time"

	"github.com/minz1/mediafixer/internal/operation"
)

func inPlanStates(s Snapshot) bool {
	if s.Status == ExecutingPlan {
		return true
	}
	return s.Status == ManualTestNeeded && (s.Reason == ReasonPlanFailed || s.Reason == ReasonPlanPartial ||
		s.Reason == ReasonPlanInterrupted)
}

func scopeOp(next Snapshot) Snapshot {
	if next.Op != nil && !inPlanStates(next) {
		next.Op = nil
	}
	return next
}

func cloneOp(op operation.Op) *operation.Op {
	op.Results = slices.Clone(op.Results)
	return &op
}

func opNotice(body, err string) Notice {
	return Notice{Kind: NoticePlanInterrupted, Body: body, Err: err}
}

func stepApprove(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	switch {
	case cur.Plan == nil:
		return Decision{Next: cur}, ErrNoPlan
	case !cur.PlanFresh(now):
		return Decision{Next: cur}, ErrPlanExpired
	case cur.Plan.FP != ev.PlanFP:
		return Decision{Next: cur}, ErrPlanChanged
	case cur.In.OpRunning:
		return Decision{Next: cur}, ErrOpRunning
	case ev.BuildErr != "":
		next := cur
		next.Op = nil
		return toOwner(cur, next, ReasonPlanFailed, opNotice("", ev.BuildErr))
	case ev.Spec == nil:
		return Decision{Next: cur}, ErrNoSpec
	}
	return approveSpec(cur, ev, now)
}

func approveSpec(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	op, err := operation.Next(operation.Op{}, operation.Event{Kind: operation.Created, At: now, Spec: ev.Spec})
	if err != nil {
		return Decision{Next: cur}, err
	}
	next := cur
	enter(&next, ExecutingPlan)
	gen := bump(&next)
	next.Op = cloneOp(op)
	d, err := moved(next, Effect{Kind: ExecutePlan, Gen: gen, OpID: op.ID})
	d.OpWrite, d.OpNew = cloneOp(op), true
	return d, err
}

func approvedVerify(cur Snapshot, now time.Time) (Decision, error) {
	next := cur
	next.Op = nil
	spec := VerifySpec{
		Kind: VerifyGeneric, ItemID: cur.ItemID, Action: cur.Finding.EscalateAction, ETAMinutes: planETAMinutes,
	}
	delay := next.In.Limits.ApprovedVerify
	if cur.What == WhatStuckRead {
		spec.Kind = VerifyStuckRead
		spec.VerifyEnteredAt = now
		delay = max(delay, next.In.Limits.StuckReadFloor)
	}
	enter(&next, Verifying)
	next.NextCheck = now.Add(delay)
	next.Verify = &Verify{ChecksLeft: max(next.In.Limits.VerifyChecks, 1), Spec: spec}
	gen := bump(&next)
	return moved(next, check(gen, &next.Verify.Spec.CheckSeq, next.NextCheck, CheckVerify))
}

func opFailed(cur Snapshot) (Decision, error) {
	op := *cur.Op
	reason := ReasonPlanFailed
	if operation.Applied(op) {
		reason = ReasonPlanPartial
	}
	errText := ""
	if op.Index >= 0 && op.Index < len(op.Results) {
		errText = op.Results[op.Index].Err
	}
	return toOwner(cur, cur, reason, opNotice(operation.Line(op), errText))
}

func stepPlanExecuted(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	op := cur.Op
	if op == nil || op.ID != ev.OpID {
		return Decision{Next: cur}, ErrOpMismatch
	}
	switch op.Status {
	case operation.Done:
		return approvedVerify(cur, now)
	case operation.FailedAtStep:
		return opFailed(cur)
	case operation.None, operation.Running:
	}
	return Decision{Next: cur}, ErrOpNotFinished
}

func stepPlanLost(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	op := cur.Op
	switch {
	case op == nil:
		return toOwner(cur, cur, ReasonPlanInterrupted, Notice{Kind: NoticePlanInterrupted})
	case op.Status == operation.Done:
		return approvedVerify(cur, now)
	case op.Status == operation.FailedAtStep && op.Cause == operation.CauseError:
		return opFailed(cur)
	}
	return lostMidway(cur, ev, now)
}

func lostMidway(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	next := cur
	var write *operation.Op
	if cur.Op.Status == operation.Running {
		cause := operation.CauseRestart
		if ev.Cause == operation.CauseStore {
			cause = operation.CauseStore
		}
		stopped, err := operation.Next(*cur.Op, operation.Event{Kind: operation.Interrupted, At: now, Cause: cause})
		if err != nil {
			return Decision{Next: cur}, err
		}
		next.Op, write = cloneOp(stopped), cloneOp(stopped)
	}
	reason := ReasonPlanFailed
	if operation.Applied(*next.Op) {
		reason = ReasonPlanInterrupted
	}
	d, err := toOwner(cur, next, reason, opNotice(interruptedText(*next.Op), ""))
	d.OpWrite = write
	return d, err
}

func interruptedText(op operation.Op) string {
	cause := "restart"
	if op.Cause == operation.CauseStore {
		cause = "database error"
	}
	if !operation.Applied(op) {
		return fmt.Sprintf(
			"the approved plan was interrupted by a %s before it changed anything; preview it again.",
			cause,
		)
	}
	return fmt.Sprintf("the approved plan was interrupted by a %s at step %d of %d. Resume continues from there.",
		cause, op.Index+1, len(op.Results))
}

func stepResumeOp(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	op := cur.Op
	switch {
	case cur.Reason != ReasonPlanPartial && cur.Reason != ReasonPlanInterrupted:
		return Decision{Next: cur}, ErrNotResumable
	case op == nil || op.ID != ev.OpID || op.Version != ev.OpVersion:
		return Decision{Next: cur}, ErrChanged
	}
	cont, err := operation.Next(*op, operation.Event{
		Kind: operation.OwnerContinued, At: now, Version: ev.OpVersion, Probes: ev.Probes,
	})
	if err != nil {
		return Decision{Next: cur}, err
	}
	next := cur
	if cont.Status == operation.Done {
		d, derr := approvedVerify(cur, now)
		d.OpWrite = cloneOp(cont)
		return d, derr
	}
	next.Plan = &Plan{JSON: append([]byte(nil), op.Source...), At: now, FP: ev.PlanFP}
	next.Op = cloneOp(cont)
	enter(&next, ExecutingPlan)
	gen := bump(&next)
	d, err := moved(next, Effect{Kind: ExecutePlan, Gen: gen, OpID: cont.ID})
	d.OpWrite = cloneOp(cont)
	return d, err
}
