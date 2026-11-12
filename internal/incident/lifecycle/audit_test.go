package lifecycle_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func mustApply(t *testing.T, s lifecycle.Snapshot, e tev) lifecycle.Decision {
	t.Helper()
	d, err := apply(s, e)
	if err != nil {
		t.Fatalf("%s from %s: %v", e.ev.Kind, s.Status, err)
	}
	if cerr := lifecycle.Check(d.Next); cerr != nil {
		t.Fatalf("Check: %v", cerr)
	}
	return d
}

func kinds(s lifecycle.Snapshot) []lifecycle.ActionKind {
	var out []lifecycle.ActionKind
	for _, a := range lifecycle.Actions(s, base()) {
		out = append(out, a.Kind)
	}
	return out
}

func hasEffect(d lifecycle.Decision, k lifecycle.EffectKind) bool {
	return slices.ContainsFunc(d.Effects, func(e lifecycle.Effect) bool { return e.Kind == k })
}

func TestI1_19_RerunRefusedWhilePlanExecutes(t *testing.T) {
	t.Parallel()
	_, err := apply(snap(lifecycle.ExecutingPlan), ev(lifecycle.OwnerRerun))
	if !errors.Is(err, lifecycle.ErrPlanRunning) {
		t.Fatalf("rerun during plan: %v", err)
	}
	if slices.Contains(kinds(snap(lifecycle.ExecutingPlan)), lifecycle.ActRerun) {
		t.Fatal("rerun offered while the plan runs")
	}
}

func TestI3_8_OwnerActionsDuringPlanAreRefusedOrSafe(t *testing.T) {
	t.Parallel()
	x := snap(lifecycle.ExecutingPlan)
	for _, k := range []lifecycle.EventKind{
		lifecycle.OwnerKeepSearching, lifecycle.OwnerPreview, lifecycle.OwnerApprove, lifecycle.OwnerRerun,
		lifecycle.OwnerResolve,
	} {
		if _, err := apply(x, ev(k)); err == nil {
			t.Errorf("%s accepted during the plan", k)
		}
	}
	u := mustApply(t, with(x, locked), ev(lifecycle.OwnerUnlock))
	if u.Next.Status != lifecycle.ExecutingPlan || u.Next.Locked || len(u.Effects) != 0 {
		t.Fatalf("unlock during plan: %+v", u)
	}
}

func TestI2_17_KeepSearchingOnlyForNoRelease(t *testing.T) {
	t.Parallel()
	for _, r := range lifecycle.Reasons() {
		s := with(snap(lifecycle.ManualTestNeeded), reason(r))
		offered := slices.Contains(kinds(s), lifecycle.ActKeepSearching)
		_, err := apply(s, ev(lifecycle.OwnerKeepSearching))
		want := r == lifecycle.ReasonNoRelease
		if offered != want || (err == nil) != want {
			t.Errorf("%s: offered=%v accepted=%v", r, offered, err == nil)
		}
	}
}

func TestI3_7_KeepSearchingDropsPlan(t *testing.T) {
	t.Parallel()
	s := with(
		snap(lifecycle.ManualTestNeeded),
		reason(lifecycle.ReasonNoRelease),
		finding(escalatable()),
		planAt(base()),
	)
	d := mustApply(t, s, ev(lifecycle.OwnerKeepSearching))
	if d.Next.Status != lifecycle.AwaitingDownload || d.Next.Plan != nil || d.Next.Reason != lifecycle.ReasonNone {
		t.Fatalf("keep searching kept %s plan=%v reason=%s", d.Next.Status, d.Next.Plan, d.Next.Reason)
	}
	if !d.Next.Download.KeepSearching || d.Next.NextCheck.IsZero() {
		t.Fatal("keep searching not armed")
	}
}

func TestI1_13_UnlockBlockedOpensAndLaunches(t *testing.T) {
	t.Parallel()
	d := mustApply(t, snap(lifecycle.Blocked), ev(lifecycle.OwnerUnlock))
	if d.Next.Status != lifecycle.Open || d.Next.Locked || !hasEffect(d, lifecycle.LaunchRun) {
		t.Fatalf("unlock blocked: %+v", d)
	}
}

func TestI4_5_UnlockBlockedLeavesBlocked(t *testing.T) {
	t.Parallel()
	for _, p := range []bool{false, true} {
		d := mustApply(t, snap(lifecycle.Blocked), tev{ev: lifecycle.Event{Kind: lifecycle.OwnerUnlock}, paused: p})
		if d.Next.Status == lifecycle.Blocked {
			t.Fatalf("paused=%v: unlock left the incident blocked", p)
		}
		if p && (!d.Next.Parked || hasEffect(d, lifecycle.LaunchRun)) {
			t.Fatal("unlock while paused launched instead of parking")
		}
	}
}

func TestA1_8_OwnerRerunResetsBudget(t *testing.T) {
	t.Parallel()
	for _, s := range lifecycle.States() {
		if s == lifecycle.ExecutingPlan {
			continue
		}
		d := mustApply(t, with(snap(s), locked), ev(lifecycle.OwnerRerun))
		if d.Next.ActionCount != 0 || d.Next.Locked {
			t.Errorf("%s: rerun kept count %d locked %v", s, d.Next.ActionCount, d.Next.Locked)
		}
	}
	sys := mustApply(t, with(snap(lifecycle.AgentFixed), locked), ev(lifecycle.SystemRerun))
	if sys.Next.ActionCount != 3 || !sys.Next.Locked {
		t.Fatal("system rerun reset the budget")
	}
}

func TestA1_9_UnlockResetsBudget(t *testing.T) {
	t.Parallel()
	for _, s := range lifecycle.States() {
		if s == lifecycle.Resolved {
			continue
		}
		d := mustApply(t, with(snap(s), locked), ev(lifecycle.OwnerUnlock))
		if d.Next.ActionCount != 0 || d.Next.Locked {
			t.Errorf("%s: unlock kept count %d locked %v", s, d.Next.ActionCount, d.Next.Locked)
		}
	}
}

func TestI2_9_QueueDropAfterGrabWaitsForImport(t *testing.T) {
	t.Parallel()
	s := with(snap(lifecycle.AwaitingDownload), dl(download(lifecycle.StageDownloading, true, false)))
	d := mustApply(t, s, obs(lifecycle.Observation{}, false))
	if d.Next.Status != lifecycle.AwaitingDownload || d.Next.Download.Stage != lifecycle.StageImporting {
		t.Fatalf("queue drop after grab: %s %v", d.Next.Status, d.Next.Download)
	}
	if hasEffect(d, lifecycle.NotifyOwner) {
		t.Fatal("queue drop after grab escalated")
	}
	f := mustApply(t, d.Next, obs(lifecycle.Observation{HasFile: true}, false))
	if f.Next.Status != lifecycle.AgentFixed {
		t.Fatalf("import landed: %s", f.Next.Status)
	}
}

func TestI1_12_ScanRunningSchedulesAnotherCheck(t *testing.T) {
	t.Parallel()
	s := with(snap(lifecycle.Verifying), lastChk)
	d := mustApply(t, s, verifyEv(lifecycle.VerifyNotYet, true))
	if d.Next.Status != lifecycle.Verifying || !hasEffect(d, lifecycle.ScheduleCheck) {
		t.Fatalf("scan running: %+v", d)
	}
	if !d.Next.NextCheck.After(base()) {
		t.Fatal("no later check scheduled")
	}
	again := mustApply(t, d.Next, verifyEv(lifecycle.VerifyNotYet, true))
	if hasEffect(again, lifecycle.NotifyReporters) {
		t.Fatal("the ETA note was sent twice")
	}
}

func TestA5_10_StaleRunWhilePausedParks(t *testing.T) {
	t.Parallel()
	for _, o := range []lifecycle.Origin{lifecycle.OriginStale, lifecycle.OriginRestart} {
		e := tev{ev: lifecycle.Event{Kind: lifecycle.StaleRun, Origin: o}, paused: true}
		d := mustApply(t, snap(lifecycle.Investigating), e)
		if !d.Next.Parked || hasEffect(d, lifecycle.LaunchRun) || hasEffect(d, lifecycle.NotifyOwner) {
			t.Fatalf("%s while paused: %+v", o, d)
		}
	}
}

func TestPlanLost_DropsPlanAndOffersNoPreview(t *testing.T) {
	t.Parallel()
	d := mustApply(t, snap(lifecycle.ExecutingPlan), ev(lifecycle.PlanLost))
	if d.Next.Plan != nil || d.Next.Reason != lifecycle.ReasonPlanInterrupted {
		t.Fatalf("plan lost: %+v", d.Next)
	}
	want := []lifecycle.ActionKind{lifecycle.ActRerun, lifecycle.ActResolve}
	if got := kinds(d.Next); !slices.Equal(got, want) {
		t.Fatalf("actions %v, want %v", got, want)
	}
	if got := kinds(with(d.Next, locked)); !slices.Contains(got, lifecycle.ActUnlock) {
		t.Fatalf("locked actions %v", got)
	}
}

func TestPlanExecuted_FailedWithoutDestructiveStepOffersPreview(t *testing.T) {
	t.Parallel()
	d := mustApply(t, execOp(failedFirstOp()), executedEv(opID))
	if d.Next.Reason != lifecycle.ReasonPlanFailed || d.Next.Plan != nil {
		t.Fatalf("plan failed: %+v", d.Next)
	}
	if !slices.Contains(kinds(d.Next), lifecycle.ActPreview) {
		t.Fatalf("actions %v lack Preview", kinds(d.Next))
	}
}
