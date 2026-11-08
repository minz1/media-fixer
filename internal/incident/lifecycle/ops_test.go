package lifecycle_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

func decide(t *testing.T, s lifecycle.Snapshot, e tev) lifecycle.Decision {
	t.Helper()
	d, err := apply(s, e)
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if cerr := lifecycle.CheckDecision(d); cerr != nil {
		t.Fatalf("CheckDecision: %v", cerr)
	}
	return d
}

func rejected(t *testing.T, s lifecycle.Snapshot, e tev) error {
	t.Helper()
	d, err := apply(s, e)
	if err == nil {
		t.Fatalf("accepted into %s", d.Next.Status)
	}
	return err
}

func ownerNotice(t *testing.T, d lifecycle.Decision) lifecycle.Notice {
	t.Helper()
	for _, e := range d.Effects {
		if e.Kind == lifecycle.NotifyOwner {
			return e.Notice
		}
	}
	t.Fatalf("no owner notice in %v", effectKinds(d.Effects))
	return lifecycle.Notice{}
}

func onlyEffect(t *testing.T, d lifecycle.Decision, k lifecycle.EffectKind) lifecycle.Effect {
	t.Helper()
	var got []lifecycle.Effect
	for _, e := range d.Effects {
		if e.Kind == k {
			got = append(got, e)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d %s effects in %v", len(got), k, effectKinds(d.Effects))
	}
	return got[0]
}

func freshMtn(extra ...func(*lifecycle.Snapshot)) lifecycle.Snapshot {
	return with(snap(lifecycle.ManualTestNeeded), append([]func(*lifecycle.Snapshot){
		finding(escalatable()), planAt(base()),
	}, extra...)...)
}

func TestOwnerApprove_CreatesOpAndExecutes(t *testing.T) {
	t.Parallel()
	from := freshMtn()
	d := decide(t, from, specEv())
	n := d.Next
	if n.Status != lifecycle.ExecutingPlan || n.Plan == nil || !bytes.Equal(n.Plan.JSON, from.Plan.JSON) {
		t.Fatalf("next %s plan %v", n.Status, n.Plan)
	}
	if !d.OpNew || d.OpWrite == nil || d.OpWrite.Status != operation.Running ||
		d.OpWrite.Subject != (operation.Subject{Kind: operation.SubjectIncident, ID: "inc"}) {
		t.Fatalf("op write %+v new=%v", d.OpWrite, d.OpNew)
	}
	if n.Op == nil || n.Op.Kind != d.OpWrite.Kind || !bytes.Equal(n.Op.Source, n.Plan.JSON) {
		t.Fatalf("next op %+v", n.Op)
	}
	e := onlyEffect(t, d, lifecycle.ExecutePlan)
	if e.Gen != n.Generation || e.OpID != n.Op.ID {
		t.Errorf("effect %+v", e)
	}
	if n.Version != from.Version+1 || n.Generation != from.Generation+1 {
		t.Errorf("version %d generation %d", n.Version, n.Generation)
	}
}

func TestOwnerApprove_ReplacesTheFailedOp(t *testing.T) {
	t.Parallel()
	from := freshMtn(setOp(failedFirstOp()), reason(lifecycle.ReasonPlanFailed))
	d := decide(t, from, specEv())
	if !d.OpNew || d.Next.Op == nil || d.Next.Op.Status != operation.Running {
		t.Fatalf("next op %+v new=%v", d.Next.Op, d.OpNew)
	}
}

func TestOwnerApprove_BuildErrorIsPlanFailedWithoutOp(t *testing.T) {
	t.Parallel()
	from := freshMtn(setOp(failedFirstOp()), reason(lifecycle.ReasonPlanFailed))
	d := decide(t, from, buildErrEv())
	n := d.Next
	if n.Status != lifecycle.ManualTestNeeded || n.Reason != lifecycle.ReasonPlanFailed || n.Plan != nil ||
		n.Op != nil {
		t.Fatalf("next %s/%s plan %v op %v", n.Status, n.Reason, n.Plan, n.Op)
	}
	if d.OpWrite != nil || d.OpNew {
		t.Errorf("op write %+v new=%v", d.OpWrite, d.OpNew)
	}
	if got := effectKinds(d.Effects); !slices.Equal(got, []K{ownerK}) {
		t.Errorf("effects %v", got)
	}
	if got := ownerNotice(t, d).Err; got != "premise changed" {
		t.Errorf("notice error %q", got)
	}
	if n.Generation != from.Generation+1 || n.Version != from.Version+1 {
		t.Errorf("generation %d version %d", n.Generation, n.Version)
	}
}

func TestOwnerApprove_OpRunningRejected(t *testing.T) {
	t.Parallel()
	running := specEv()
	running.opRunning = true
	if err := rejected(t, freshMtn(), running); !errors.Is(err, lifecycle.ErrOpRunning) {
		t.Errorf("err = %v", err)
	}
	bare := tev{ev: lifecycle.Event{Kind: lifecycle.OwnerApprove}, opRunning: true}
	if err := rejected(t, freshMtn(), bare); !errors.Is(err, lifecycle.ErrOpRunning) {
		t.Errorf("bare approve err = %v", err)
	}
	if err := rejected(t, with(freshMtn(), func(s *lifecycle.Snapshot) { s.Plan = nil }), running); !errors.Is(
		err, lifecycle.ErrNoPlan) {
		t.Errorf("no plan err = %v", err)
	}
}

func TestPlanExecuted_DoneOp(t *testing.T) {
	t.Parallel()
	from := with(execOp(doneOp()), finding(escalatable()))
	d := decide(t, from, executedEv(opID))
	n := d.Next
	if n.Status != lifecycle.Verifying || n.Op != nil || n.Plan != nil || d.OpWrite != nil {
		t.Fatalf("next %s op %v plan %v write %v", n.Status, n.Op, n.Plan, d.OpWrite)
	}
	if !n.NextCheck.Equal(base().Add(limits().ApprovedVerify)) ||
		n.Verify.Spec.Kind != lifecycle.VerifyGeneric || n.Verify.Spec.ETAMinutes != 15 ||
		n.Verify.Spec.Action != lifecycle.EscalateRemoveAndSearch {
		t.Errorf("verify %+v at %v", n.Verify, n.NextCheck)
	}
	e := onlyEffect(t, d, lifecycle.ScheduleCheck)
	if e.Check != lifecycle.CheckVerify || e.Gen != n.Generation || !e.At.Equal(n.NextCheck) {
		t.Errorf("effect %+v", e)
	}
}

func TestPlanExecuted_FailedOp(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		op     operation.Op
		reason lifecycle.Reason
	}{
		"failed not applied": {failedFirstOp(), lifecycle.ReasonPlanFailed},
		"failed applied":     {failedSecondOp(), lifecycle.ReasonPlanPartial},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := decide(t, execOp(c.op), executedEv(opID))
			n := d.Next
			if n.Status != lifecycle.ManualTestNeeded || n.Reason != c.reason || n.Plan != nil {
				t.Fatalf("next %s/%s plan %v", n.Status, n.Reason, n.Plan)
			}
			if n.Op == nil || n.Op.ID != opID || n.Op.Status != operation.FailedAtStep || d.OpWrite != nil {
				t.Errorf("op %+v write %v", n.Op, d.OpWrite)
			}
			if got := ownerNotice(t, d).Body; got != operation.Line(c.op) {
				t.Errorf("notice body %q, want %q", got, operation.Line(c.op))
			}
		})
	}
}

func TestPlanExecuted_NotFinishedOrMismatched(t *testing.T) {
	t.Parallel()
	t.Run("running", func(t *testing.T) {
		t.Parallel()
		if err := rejected(t, execOp(startedOp()), executedEv(opID)); !errors.Is(err, lifecycle.ErrOpNotFinished) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		t.Parallel()
		for _, id := range []int64{0, opID + 1} {
			if err := rejected(t, execOp(doneOp()), executedEv(id)); !errors.Is(err, lifecycle.ErrOpMismatch) {
				t.Errorf("id %d: err = %v", id, err)
			}
		}
		if err := rejected(
			t,
			snap(lifecycle.ExecutingPlan),
			executedEv(opID),
		); !errors.Is(
			err,
			lifecycle.ErrOpMismatch,
		) {
			t.Errorf("no op: err = %v", err)
		}
	})
}

func TestPlanLost_InterruptsRunningOp(t *testing.T) {
	t.Parallel()
	from := execOp(startedOp())
	d := decide(t, from, lostEv(operation.CauseStore))
	n := d.Next
	if n.Status != lifecycle.ManualTestNeeded || n.Reason != lifecycle.ReasonPlanInterrupted || n.Plan != nil {
		t.Fatalf("next %s/%s plan %v", n.Status, n.Reason, n.Plan)
	}
	w := d.OpWrite
	if w == nil || d.OpNew || w.Status != operation.FailedAtStep || w.Cause != operation.CauseStore ||
		w.Results[0].State != operation.ResultUnknown {
		t.Fatalf("op write %+v new=%v", w, d.OpNew)
	}
	if n.Op == nil || n.Op.Status != operation.FailedAtStep || n.Op.Version != w.Version {
		t.Errorf("next op %+v", n.Op)
	}
	want := "the approved plan was interrupted by a database error at step 1 of 2. Resume continues from there."
	notice := ownerNotice(t, d)
	if notice.Kind != lifecycle.NoticePlanInterrupted || notice.Body != want {
		t.Errorf("notice %+v", notice)
	}
}

func TestPlanLost_NothingStartedIsPlanFailed(t *testing.T) {
	t.Parallel()
	d := decide(t, execOp(runningOp()), lostEv(operation.CauseRestart))
	n := d.Next
	if n.Status != lifecycle.ManualTestNeeded || n.Reason != lifecycle.ReasonPlanFailed || n.Plan != nil {
		t.Fatalf("next %s/%s plan %v", n.Status, n.Reason, n.Plan)
	}
	if d.OpWrite == nil || d.OpWrite.Status != operation.FailedAtStep || n.Op == nil ||
		n.Op.Status != operation.FailedAtStep {
		t.Errorf("op write %+v next op %+v", d.OpWrite, n.Op)
	}
	want := "the approved plan was interrupted by a restart before it changed anything; preview it again."
	if got := ownerNotice(t, d).Body; got != want {
		t.Errorf("body %q", got)
	}
	again := decide(t, execOp(restartNothingOp()), lostEv(operation.CauseNone))
	if again.OpWrite != nil || again.Next.Reason != lifecycle.ReasonPlanFailed || ownerNotice(t, again).Body != want {
		t.Errorf("already interrupted: write %v reason %s", again.OpWrite, again.Next.Reason)
	}
}

func TestPlanLost_DoneOpGoesToVerifying(t *testing.T) {
	t.Parallel()
	d := decide(t, with(execOp(doneOp()), finding(escalatable())), lostEv(operation.CauseNone))
	n := d.Next
	if n.Status != lifecycle.Verifying || n.Op != nil || d.OpWrite != nil {
		t.Fatalf("next %s op %v write %v", n.Status, n.Op, d.OpWrite)
	}
	e := onlyEffect(t, d, lifecycle.ScheduleCheck)
	if e.Check != lifecycle.CheckVerify || !e.At.Equal(base().Add(limits().ApprovedVerify)) {
		t.Errorf("effect %+v", e)
	}
	failed := decide(t, execOp(failedSecondOp()), lostEv(operation.CauseNone))
	if failed.Next.Reason != lifecycle.ReasonPlanPartial || failed.Next.Op == nil {
		t.Errorf("error cause: %s op %v", failed.Next.Reason, failed.Next.Op)
	}
	first := decide(t, execOp(failedFirstOp()), lostEv(operation.CauseNone))
	if first.Next.Reason != lifecycle.ReasonPlanFailed || first.Next.Op == nil {
		t.Errorf("error cause not applied: %s op %v", first.Next.Reason, first.Next.Op)
	}
}

func TestPlanLost_PreP4RowKeepsP2Behaviour(t *testing.T) {
	t.Parallel()
	d := decide(t, snap(lifecycle.ExecutingPlan), lostEv(operation.CauseNone))
	n := d.Next
	if n.Reason != lifecycle.ReasonPlanInterrupted || n.Op != nil || d.OpWrite != nil || n.Plan != nil {
		t.Fatalf("next %s op %v write %v", n.Reason, n.Op, d.OpWrite)
	}
	notice := ownerNotice(t, d)
	if notice.Kind != lifecycle.NoticePlanInterrupted || notice.Body != "" {
		t.Errorf("notice %+v", notice)
	}
	if lifecycle.Offers(n, base(), lifecycle.ActResume) {
		t.Error("resume offered without an op")
	}
}

func TestPlanLost_StoreCauseInDM(t *testing.T) {
	t.Parallel()
	for name, from := range map[string]lifecycle.Snapshot{
		"running":     execOp(startedOp()),
		"interrupted": execOp(storeAppliedOp()),
	} {
		got := ownerNotice(t, decide(t, from, lostEv(operation.CauseStore))).Body
		want := "the approved plan was interrupted by a database error at step "
		if name == "interrupted" {
			want += "2 of 2. Resume continues from there."
		} else {
			want += "1 of 2. Resume continues from there."
		}
		if got != want {
			t.Errorf("%s: body %q, want %q", name, got, want)
		}
	}
}

func TestPlanLost_EmptyCauseIsRestart(t *testing.T) {
	t.Parallel()
	d := decide(t, execOp(startedOp()), lostEv(operation.CauseNone))
	if d.OpWrite == nil || d.OpWrite.Cause != operation.CauseRestart {
		t.Fatalf("op write %+v", d.OpWrite)
	}
	want := "the approved plan was interrupted by a restart at step 1 of 2. Resume continues from there."
	if got := ownerNotice(t, d).Body; got != want {
		t.Errorf("body %q", got)
	}
}

func leaveSamples() []lifecycle.Snapshot {
	return slices.Concat(execSamples(), mtnSamples())
}

func TestIncidentMachine_LeavingPlanStatesDropsOp(t *testing.T) {
	t.Parallel()
	for _, from := range leaveSamples() {
		for _, k := range lifecycle.EventKinds() {
			for _, e := range events(k) {
				d, err := apply(from, e)
				if err != nil {
					continue
				}
				if cerr := lifecycle.Check(d.Next); cerr != nil {
					t.Errorf("%s %v: %v", key(from), e, cerr)
				}
				if !inPlanStates(d.Next) && d.Next.Op != nil {
					t.Errorf(
						"%s %v left the plan states in %s keeping op %d",
						key(from),
						e,
						d.Next.Status,
						d.Next.Op.ID,
					)
				}
			}
		}
	}
}

func TestIncidentMachine_LeavingPlanStatesIsAccepted(t *testing.T) {
	t.Parallel()
	for _, from := range mtnSamples() {
		for _, k := range []lifecycle.EventKind{lifecycle.OwnerRerun, lifecycle.OwnerResolve} {
			d := decide(t, from, ev(k))
			if d.Next.Op != nil {
				t.Errorf("%s %s kept op %d", key(from), k, d.Next.Op.ID)
			}
		}
	}
	for _, op := range []operation.Op{runningOp(), doneOp(), failedSecondOp()} {
		d := decide(t, execOp(op), ev(lifecycle.OwnerResolve))
		if d.Next.Status != lifecycle.Resolved || d.Next.Op != nil {
			t.Errorf("resolve from executing_plan: %s op %v", d.Next.Status, d.Next.Op)
		}
	}
}

func TestOwnerResumeOp_RestoresPlanAndExecutes(t *testing.T) {
	t.Parallel()
	from := mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp())
	d := decide(t, from, resumeEv())
	n := d.Next
	if n.Status != lifecycle.ExecutingPlan || n.Reason != lifecycle.ReasonNone {
		t.Fatalf("next %s/%s", n.Status, n.Reason)
	}
	if n.Plan == nil || !bytes.Equal(n.Plan.JSON, from.Op.Source) || !n.Plan.At.Equal(base()) || n.Plan.FP != "fp1" {
		t.Errorf("plan %+v", n.Plan)
	}
	if d.OpWrite == nil || d.OpNew || d.OpWrite.Status != operation.Running || n.Op == nil ||
		n.Op.Status != operation.Running || n.Op.Version != from.Op.Version+1 {
		t.Errorf("op write %+v next op %+v", d.OpWrite, n.Op)
	}
	e := onlyEffect(t, d, lifecycle.ExecutePlan)
	if e.Gen != n.Generation || e.OpID != opID {
		t.Errorf("effect %+v", e)
	}
	if n.Version != from.Version+1 || n.Generation != from.Generation+1 {
		t.Errorf("version %d generation %d", n.Version, n.Generation)
	}
}

func TestOwnerResumeOp_AllProbedDoneGoesToVerifyingWithCheck(t *testing.T) {
	t.Parallel()
	from := with(mtnOp(lifecycle.ReasonPlanPartial, failedSecondOp()))
	e := resumeEv()
	e.verdict = operation.VerdictDone
	d := decide(t, from, e)
	n := d.Next
	if n.Status != lifecycle.Verifying || n.Op != nil || n.Plan != nil {
		t.Fatalf("next %s op %v plan %v", n.Status, n.Op, n.Plan)
	}
	if d.OpWrite == nil || d.OpWrite.Status != operation.Done || d.OpNew {
		t.Errorf("op write %+v", d.OpWrite)
	}
	c := onlyEffect(t, d, lifecycle.ScheduleCheck)
	if c.Check != lifecycle.CheckVerify || !c.At.Equal(base().Add(limits().ApprovedVerify)) || c.Gen != n.Generation {
		t.Errorf("effect %+v", c)
	}
}

func TestOwnerResumeOp_StaleOpVersionRejected(t *testing.T) {
	t.Parallel()
	from := mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp())
	stale := resumeEv()
	stale.badOp = true
	if err := rejected(t, from, stale); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("op version: %v", err)
	}
	old := resumeEv()
	old.badVer = true
	if err := rejected(t, from, old); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("incident version: %v", err)
	}
}

func TestOwnerResumeOp_ConflictRejectedWithNote(t *testing.T) {
	t.Parallel()
	from := mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp())
	e := resumeEv()
	e.verdict = operation.VerdictConflict
	err := rejected(t, from, e)
	re, ok := errors.AsType[*operation.RejectError](err)
	if !ok || re.Code != operation.CodeConflict || re.Note != "probe note" {
		t.Errorf("err = %v", err)
	}
}

func TestOwnerResumeOp_NotResumableReason(t *testing.T) {
	t.Parallel()
	for _, r := range lifecycle.Reasons() {
		if r == lifecycle.ReasonPlanPartial || r == lifecycle.ReasonPlanInterrupted {
			continue
		}
		from := with(mtnOp(r, restartAppliedOp()))
		if err := rejected(t, from, resumeEv()); !errors.Is(err, lifecycle.ErrNotResumable) {
			t.Errorf("%s: err = %v", r, err)
		}
	}
	notApplied := mtnOp(lifecycle.ReasonPlanPartial, restartNothingOp())
	err := rejected(t, notApplied, resumeEv())
	if operation.CodeOf(err) != operation.CodeNotResumable {
		t.Errorf("not applied: err = %v", err)
	}
}

func TestOwnerResumeOp_AcceptedWhilePaused(t *testing.T) {
	t.Parallel()
	from := with(mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp()), locked)
	e := resumeEv()
	e.paused = true
	d := decide(t, from, e)
	if d.Next.Status != lifecycle.ExecutingPlan {
		t.Errorf("next %s", d.Next.Status)
	}
}

func TestCheck_OpOnlyInPlanStates(t *testing.T) {
	t.Parallel()
	for _, s := range lifecycle.States() {
		for _, sn := range samples(s) {
			if sn.Op != nil {
				continue
			}
			got := lifecycle.Check(with(sn, setOp(runningOp())))
			if want := !inPlanStates(sn); (got != nil) != want {
				t.Errorf("%s: err = %v, want error %v", key(sn), got, want)
			}
		}
	}
}

func TestCheck_ExecutingPlanOpSourceIsPlan(t *testing.T) {
	t.Parallel()
	good := execOp(runningOp())
	if err := lifecycle.Check(good); err != nil {
		t.Fatalf("matching source: %v", err)
	}
	bad := with(good, func(s *lifecycle.Snapshot) { s.Op.Source = []byte(`{"other":true}`) })
	if err := lifecycle.Check(bad); !errors.Is(err, lifecycle.ErrIllegal) {
		t.Errorf("source differs: %v", err)
	}
}

func TestCheckDecision_OpWriteSubjectAndShape(t *testing.T) {
	t.Parallel()
	d := decide(t, freshMtn(), specEv())
	other := *d.OpWrite
	other.Subject.ID = "someone-else"
	d.OpWrite = &other
	if err := lifecycle.CheckDecision(d); err == nil {
		t.Error("op write for another subject passed")
	}
	broken := *d.OpWrite
	broken.Subject.ID = "inc"
	broken.Results = nil
	d.OpWrite = &broken
	if err := lifecycle.CheckDecision(d); err == nil {
		t.Error("illegal op write passed")
	}
}

func TestActions_ResumeOfferedOnlyWhenResumable(t *testing.T) {
	t.Parallel()
	for _, s := range states() {
		for _, sn := range samples(s) {
			resumable := sn.Status == lifecycle.ManualTestNeeded &&
				(sn.Reason == lifecycle.ReasonPlanPartial || sn.Reason == lifecycle.ReasonPlanInterrupted) &&
				sn.Op != nil && operation.Resumable(*sn.Op)
			if got := lifecycle.Offers(sn, base(), lifecycle.ActResume); got != resumable {
				t.Errorf("%s: offered %v, want %v", key(sn), got, resumable)
			}
		}
	}
	got := lifecycle.Actions(mtnOp(lifecycle.ReasonPlanPartial, failedSecondOp()), base())
	for _, a := range got {
		if a.Version != 7 {
			t.Errorf("action %s carries version %d", a.Kind, a.Version)
		}
	}
}

func TestActions_ResumeIsNormalStyle(t *testing.T) {
	t.Parallel()
	from := mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp())
	var kinds []lifecycle.ActionKind
	for _, a := range lifecycle.Actions(from, base()) {
		kinds = append(kinds, a.Kind)
	}
	want := []lifecycle.ActionKind{lifecycle.ActResume, lifecycle.ActRerun, lifecycle.ActResolve}
	for _, k := range want {
		if !slices.Contains(kinds, k) {
			t.Errorf("%s missing from %v", k, kinds)
		}
	}
	for _, k := range []lifecycle.ActionKind{lifecycle.ActApprove, lifecycle.ActPreview, lifecycle.ActRefresh} {
		if slices.Contains(kinds, k) {
			t.Errorf("%s offered beside resume", k)
		}
	}
	if lifecycle.ActResume.Event() != lifecycle.OwnerResumeOp {
		t.Errorf("event %s", lifecycle.ActResume.Event())
	}
	failed := mtnOp(lifecycle.ReasonPlanFailed, restartNothingOp())
	if !lifecycle.Offers(failed, base(), lifecycle.ActPreview) ||
		lifecycle.Offers(failed, base(), lifecycle.ActResume) {
		t.Error("plan_failed must keep preview and offer no resume")
	}
}

func TestOwnerResumeOp_WrongOpIDRejected(t *testing.T) {
	t.Parallel()
	cur, _ := prepare(mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp()), resumeEv())
	e := lifecycle.Event{
		Kind: lifecycle.OwnerResumeOp, Version: cur.Version, HasVersion: true,
		OpID: cur.Op.ID + 1, OpVersion: cur.Op.Version,
	}
	if _, err := lifecycle.Next(cur, e, base()); !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("err = %v", err)
	}
}

func TestOwnerResumeOp_NoOpRejected(t *testing.T) {
	t.Parallel()
	for _, r := range []lifecycle.Reason{lifecycle.ReasonPlanPartial, lifecycle.ReasonPlanInterrupted} {
		from := with(snap(lifecycle.ManualTestNeeded), finding(escalatable()), reason(r))
		if err := rejected(t, from, resumeEv()); !errors.Is(err, lifecycle.ErrChanged) {
			t.Errorf("%s: err = %v", r, err)
		}
	}
}

func TestOwnerApprove_WithoutSpecIsRejected(t *testing.T) {
	t.Parallel()
	bare := ev(lifecycle.OwnerApprove)
	if err := rejected(t, freshMtn(), bare); !errors.Is(err, lifecycle.ErrNoSpec) {
		t.Errorf("err = %v", err)
	}
}

func TestPlanExecuted_WithoutOpIsRejected(t *testing.T) {
	t.Parallel()
	for _, e := range []tev{
		ev(lifecycle.PlanExecuted),
		{ev: lifecycle.Event{Kind: lifecycle.PlanExecuted, Err: "boom"}},
	} {
		if err := rejected(t, snap(lifecycle.ExecutingPlan), e); !errors.Is(err, lifecycle.ErrOpMismatch) {
			t.Errorf("err = %v", err)
		}
	}
}
