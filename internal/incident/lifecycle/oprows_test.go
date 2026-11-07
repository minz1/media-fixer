package lifecycle_test

import (
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

func specEv() tev {
	spec := opSpec()
	return tev{ev: lifecycle.Event{Kind: lifecycle.OwnerApprove, Spec: &spec}}
}

func buildErrEv() tev {
	return tev{ev: lifecycle.Event{Kind: lifecycle.OwnerApprove, BuildErr: "premise changed"}}
}

func executedEv(id int64) tev {
	return tev{ev: lifecycle.Event{Kind: lifecycle.PlanExecuted, OpID: id}}
}

func lostEv(c operation.Cause) tev {
	return tev{ev: lifecycle.Event{Kind: lifecycle.PlanLost, Cause: c}}
}

func resumeEv() tev { return tev{ev: lifecycle.Event{Kind: lifecycle.OwnerResumeOp, PlanFP: "fp1"}} }

func approveOpRows() []docRow {
	mtn := with(snap(lifecycle.ManualTestNeeded), finding(escalatable()))
	fresh := with(mtn, planAt(base()))
	running, late, badFP := specEv(), specEv(), specEv()
	running.opRunning, late.late, badFP.badFP = true, true, true
	return []docRow{
		{name: "approve spec no plan", from: mtn, ev: specEv(), err: lifecycle.ErrNoPlan},
		{name: "approve spec expired", from: fresh, ev: late, err: lifecycle.ErrPlanExpired},
		{name: "approve spec changed", from: fresh, ev: badFP, err: lifecycle.ErrPlanChanged},
		{name: "approve op running", from: fresh, ev: running, err: lifecycle.ErrOpRunning},
		{name: "approve build error", from: fresh, ev: buildErrEv(), to: lifecycle.ManualTestNeeded, gen: true,
			effs: []K{ownerK}, assert: reasonIs(lifecycle.ReasonPlanFailed)},
		{name: "approve spec", from: fresh, ev: specEv(), to: lifecycle.ExecutingPlan, gen: true, effs: []K{planK}},
		{
			name: "approve spec replaces the failed op",
			from: with(fresh, setOp(failedFirstOp()), reason(lifecycle.ReasonPlanFailed)),
			ev:   specEv(),
			to:   lifecycle.ExecutingPlan,
			gen:  true,
			effs: []K{planK},
		},
		{name: "approve spec open", from: snap(lifecycle.Open), ev: specEv(), err: lifecycle.ErrNotAwaitingDecision},
	}
}

func executedOpRows() []docRow {
	return []docRow{
		{name: "plan op nil mismatch", from: snap(lifecycle.ExecutingPlan), ev: executedEv(opID),
			err: lifecycle.ErrOpMismatch},
		{name: "plan op mismatch", from: execOp(runningOp()), ev: executedEv(opID + 1), err: lifecycle.ErrOpMismatch},
		{name: "plan op zero id mismatch", from: execOp(runningOp()), ev: executedEv(0), err: lifecycle.ErrOpMismatch},
		{name: "plan op running", from: execOp(startedOp()), ev: executedEv(opID), err: lifecycle.ErrOpNotFinished},
		{name: "plan op done", from: execOp(doneOp()), ev: executedEv(opID), to: lifecycle.Verifying, gen: true,
			effs: []K{checkK}},
		{name: "plan op failed not applied", from: execOp(failedFirstOp()), ev: executedEv(opID),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanFailed)},
		{name: "plan op failed applied", from: execOp(failedSecondOp()), ev: executedEv(opID),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanPartial)},
		{name: "plan op stale", from: mtnOp(lifecycle.ReasonPlanFailed, failedFirstOp()), ev: executedEv(opID),
			err: lifecycle.ErrStale},
	}
}

func lostOpRows() []docRow {
	return []docRow{
		{
			name: "plan lost done op",
			from: execOp(doneOp()),
			ev:   lostEv(operation.CauseNone),
			to:   lifecycle.Verifying,
			gen:  true,
			effs: []K{checkK},
		},
		{name: "plan lost failed op not applied", from: execOp(failedFirstOp()), ev: lostEv(operation.CauseNone),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanFailed)},
		{name: "plan lost failed op applied", from: execOp(failedSecondOp()), ev: lostEv(operation.CauseNone),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanPartial)},
		{name: "plan lost running applied", from: execOp(startedOp()), ev: lostEv(operation.CauseStore),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanInterrupted)},
		{name: "plan lost running nothing started", from: execOp(runningOp()), ev: lostEv(operation.CauseRestart),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanFailed)},
		{name: "plan lost interrupted applied", from: execOp(restartAppliedOp()), ev: lostEv(operation.CauseNone),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanInterrupted)},
		{name: "plan lost interrupted nothing", from: execOp(restartNothingOp()), ev: lostEv(operation.CauseNone),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK},
			assert: reasonIs(lifecycle.ReasonPlanFailed)},
	}
}

func resumeOpRows() []docRow {
	partial := mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp())
	badOp, badVer, done := resumeEv(), resumeEv(), resumeEv()
	badOp.badOp, badVer.badVer, done.verdict = true, true, operation.VerdictDone
	return []docRow{
		{name: "resume other reason", from: mtnOp(lifecycle.ReasonPlanFailed, restartNothingOp()), ev: resumeEv(),
			err: lifecycle.ErrNotResumable},
		{name: "resume needs owner", from: snap(lifecycle.ManualTestNeeded), ev: resumeEv(),
			err: lifecycle.ErrNotResumable},
		{name: "resume no op", from: with(snap(lifecycle.ManualTestNeeded), reason(lifecycle.ReasonPlanPartial)),
			ev: resumeEv(), err: lifecycle.ErrChanged},
		{name: "resume op version", from: partial, ev: badOp, err: lifecycle.ErrChanged},
		{name: "resume incident version", from: partial, ev: badVer, err: lifecycle.ErrChanged},
		{name: "resume", from: partial, ev: resumeEv(), to: lifecycle.ExecutingPlan, gen: true, effs: []K{planK}},
		{name: "resume paused", from: partial, ev: tev{ev: resumeEv().ev, paused: true}, to: lifecycle.ExecutingPlan,
			gen: true, effs: []K{planK}},
		{name: "resume locked", from: with(partial, locked), ev: resumeEv(), to: lifecycle.ExecutingPlan,
			gen: true, effs: []K{planK}},
		{name: "resume all done", from: partial, ev: done, to: lifecycle.Verifying, gen: true, effs: []K{checkK}},
		{name: "resume open", from: snap(lifecycle.Open), ev: resumeEv(), err: lifecycle.ErrNotAwaitingDecision},
		{name: "resume executing", from: execOp(runningOp()), ev: resumeEv(), err: lifecycle.ErrNotAwaitingDecision},
		{name: "resume new incident", from: lifecycle.Snapshot{ID: "inc"}, ev: resumeEv(), err: lifecycle.ErrStale},
	}
}
