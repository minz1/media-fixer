package lifecycle

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

const None State = ""

type step func(cur Snapshot, ev Event, now time.Time) (Decision, error)

type row struct {
	from []State
	kind EventKind
	step step
}

func except(out ...State) []State {
	var in []State
	for _, s := range States() {
		if !slices.Contains(out, s) {
			in = append(in, s)
		}
	}
	return in
}

func transitions() []row {
	launchable := []State{Open, Reopened}
	return []row{
		{[]State{None}, Reported, stepReported},
		{States(), Reported, rejectWith(ErrNotNew)},
		{[]State{None}, RunClaimed, rejectWith(ErrStale)},
		{[]State{None}, ActionAttempted, rejectWith(ErrStale)},
		{[]State{None}, AttemptFinished, rejectWith(ErrStale)},
		{[]State{None}, RunFinished, rejectWith(ErrStale)},
		{[]State{None}, VerifyObserved, rejectWith(ErrStale)},
		{[]State{None}, DownloadObserved, rejectWith(ErrStale)},
		{[]State{None}, PlanExecuted, rejectWith(ErrStale)},
		{[]State{None}, OwnerRerun, rejectWith(ErrStale)},
		{[]State{None}, OwnerResolve, rejectWith(ErrStale)},
		{[]State{None}, OwnerUnlock, rejectWith(ErrStale)},
		{[]State{None}, OwnerKeepSearching, rejectWith(ErrStale)},
		{[]State{None}, OwnerPreview, rejectWith(ErrStale)},
		{[]State{None}, OwnerApprove, rejectWith(ErrStale)},
		{[]State{None}, SystemRerun, rejectWith(ErrStale)},
		{[]State{None}, StaleRun, rejectWith(ErrStale)},
		{[]State{None}, Relaunch, rejectWith(ErrStale)},
		{[]State{None}, Resumed, rejectWith(ErrStale)},
		{[]State{None}, PlanLost, rejectWith(ErrStale)},
		{[]State{None}, OwnerResumeOp, rejectWith(ErrStale)},
		{[]State{None}, Tick, rejectWith(ErrNoop)},

		{launchable, RunClaimed, stepClaim},
		{[]State{Investigating}, RunClaimed, journalOnly},
		{except(Open, Reopened, Investigating), RunClaimed, rejectWith(ErrStale)},

		{[]State{Investigating}, ActionAttempted, stepAction},
		{except(Investigating), ActionAttempted, rejectWith(ErrStale)},

		{[]State{Investigating}, AttemptFinished, stepAttemptFinished},
		{except(Investigating), AttemptFinished, journalOnly},

		{[]State{Investigating}, RunFinished, stepRunFinished},
		{launchable, RunFinished, stepRunFinishedUnclaimed},
		{except(Open, Reopened, Investigating), RunFinished, rejectWith(ErrStale)},

		{[]State{Verifying}, VerifyObserved, stepVerifyObserved},
		{except(Verifying), VerifyObserved, rejectWith(ErrStale)},

		{[]State{AwaitingDownload}, DownloadObserved, stepDownloadObserved},
		{except(AwaitingDownload), DownloadObserved, rejectWith(ErrStale)},

		{[]State{ExecutingPlan}, PlanExecuted, stepPlanExecuted},
		{except(ExecutingPlan), PlanExecuted, rejectWith(ErrStale)},

		{except(ExecutingPlan), OwnerRerun, stepOwnerRerun},
		{[]State{ExecutingPlan}, OwnerRerun, rejectWith(ErrPlanRunning)},

		{except(Resolved, ExecutingPlan), OwnerResolve, stepOwnerResolve},
		{[]State{ExecutingPlan}, OwnerResolve, rejectWith(ErrPlanRunning)},
		{[]State{Resolved}, OwnerResolve, rejectWith(ErrAlreadyResolved)},

		{[]State{Blocked}, OwnerUnlock, stepUnlockBlocked},
		{except(Blocked, Resolved), OwnerUnlock, stepUnlock},
		{[]State{Resolved}, OwnerUnlock, rejectWith(ErrResolved)},

		{[]State{ManualTestNeeded}, OwnerKeepSearching, stepKeepSearching},
		{except(ManualTestNeeded), OwnerKeepSearching, rejectWith(ErrNoKeepSearching)},

		{[]State{ManualTestNeeded}, OwnerPreview, stepPreview},
		{except(ManualTestNeeded), OwnerPreview, rejectWith(ErrNotAwaitingDecision)},

		{[]State{ManualTestNeeded}, OwnerApprove, stepApprove},
		{except(ManualTestNeeded), OwnerApprove, rejectWith(ErrNotAwaitingDecision)},

		{[]State{ManualTestNeeded}, OwnerResumeOp, stepResumeOp},
		{except(ManualTestNeeded), OwnerResumeOp, rejectWith(ErrNotAwaitingDecision)},

		{[]State{AgentFixed}, SystemRerun, stepSystemRerun},
		{except(AgentFixed), SystemRerun, rejectWith(ErrStale)},

		{[]State{Investigating}, StaleRun, stepStaleRun},
		{except(Investigating), StaleRun, rejectWith(ErrStale)},

		{launchable, Relaunch, stepRelaunch},
		{except(Open, Reopened), Relaunch, rejectWith(ErrStale)},

		{launchable, Resumed, stepResumed},
		{except(Open, Reopened), Resumed, rejectWith(ErrStale)},

		{[]State{ExecutingPlan}, PlanLost, stepPlanLost},
		{except(ExecutingPlan), PlanLost, rejectWith(ErrStale)},

		{[]State{ManualTestNeeded}, Tick, stepTick},
		{except(ManualTestNeeded), Tick, rejectWith(ErrNoop)},
	}
}

func Next(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	if workerEvent(ev.Kind) && ev.Gen != cur.Generation {
		if ev.Kind == AttemptFinished && cur.Status != None {
			return Decision{Next: cur, JournalOnly: true}, nil
		}
		return Decision{Next: cur}, ErrStale
	}
	if versionChecked(ev.Kind) && cur.Status != None && (!ev.HasVersion || ev.Version != cur.Version) {
		return Decision{Next: cur}, ErrChanged
	}
	for _, r := range transitions() {
		if r.kind == ev.Kind && slices.Contains(r.from, cur.Status) {
			d, err := r.step(cur.Clone(), ev, now)
			if err != nil {
				return Decision{Next: cur}, err
			}
			d.Next = scopeOp(d.Next)
			return d, nil
		}
	}
	return Decision{Next: cur}, fmt.Errorf("lifecycle: no row for %s × %s", cur.Status, ev.Kind)
}

func Matches(s State, k EventKind) int {
	n := 0
	for _, r := range transitions() {
		if r.kind == k && slices.Contains(r.from, s) {
			n++
		}
	}
	return n
}

func rejectWith(err error) step {
	return func(cur Snapshot, _ Event, _ time.Time) (Decision, error) { return Decision{Next: cur}, err }
}

func journalOnly(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	return Decision{Next: cur, JournalOnly: true}, nil
}

func moved(next Snapshot, effs ...Effect) (Decision, error) {
	return Decision{Next: next, Effects: effs}, nil
}

func enter(s *Snapshot, st State) {
	s.Status = st
	if st != Verifying && st != AwaitingDownload {
		s.NextCheck = time.Time{}
	}
	if st != Verifying {
		s.Verify = nil
	}
	if st != AwaitingDownload && (st != ManualTestNeeded || s.Reason != ReasonNoRelease) {
		s.Download = nil
	}
	if st != ManualTestNeeded && st != ExecutingPlan {
		s.Plan = nil
	}
	if st != ManualTestNeeded {
		s.Reason = ReasonNone
	}
	if st != Open && st != Reopened {
		s.Parked = false
	}
}

func bump(s *Snapshot) int64 {
	s.Generation++
	return s.Generation
}

func launch(gen int64, seed Seed, origin Origin) Effect {
	return Effect{Kind: LaunchRun, Gen: gen, Seed: seed, Origin: origin}
}

func cancel(gen int64) Effect { return Effect{Kind: CancelRun, Gen: gen} }

func check(gen int64, seq *int, at time.Time, k CheckKind) Effect {
	*seq++
	return Effect{Kind: ScheduleCheck, Gen: gen, Seq: *seq, At: at, Check: k}
}

func note(n Note) Effect { return Effect{Kind: NotifyReporters, Note: n} }

func owner(n Notice) Effect { return Effect{Kind: NotifyOwner, Notice: n} }

func seedFor(s State) Seed {
	if s == Reopened {
		return SeedSummary
	}
	return SeedFresh
}

func toOwner(cur, next Snapshot, reason Reason, n Notice) (Decision, error) {
	next.Reason = reason
	next.Plan = nil
	enter(&next, ManualTestNeeded)
	bump(&next)
	var effs []Effect
	if n.Kind != NoticeNone {
		effs = append(effs, owner(n))
	}
	if cur.Status != AgentFixed && cur.Status != ExecutingPlan && cur.Status != ManualTestNeeded {
		effs = append(effs, note(NoteHanded))
	}
	return moved(next, effs...)
}

func toFixed(next Snapshot) (Decision, error) {
	enter(&next, AgentFixed)
	bump(&next)
	return moved(next, note(NoteFixed), Effect{Kind: SeerrResolve})
}

func stepReported(cur Snapshot, ev Event, _ time.Time) (Decision, error) {
	next := Snapshot{ID: cur.ID, In: cur.In}
	if ev.Report != nil {
		next.What, next.ItemID = ev.Report.What, ev.Report.ItemID
	}
	switch {
	case cur.In.ActiveCount >= cur.In.Limits.SystemicThreshold:
		next.Status, next.Locked = Blocked, true
		return moved(next, owner(Notice{Kind: NoticeSystemic}))
	case cur.In.Paused:
		next.Status, next.Parked = Open, true
		return moved(next)
	}
	next.Status, next.Generation = Open, 1
	return moved(next, launch(1, SeedFresh, OriginReport))
}

func stepClaim(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	switch {
	case cur.Parked:
		return Decision{Next: cur}, ErrParked
	case cur.In.Paused:
		return Decision{Next: cur}, ErrPaused
	}
	enter(&cur, Investigating)
	return moved(cur)
}

func stepAction(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	switch {
	case cur.In.Paused:
		return Decision{Next: cur}, ErrPaused
	case cur.Locked:
		return Decision{Next: cur}, ErrDiagnosisOnly
	case cur.ActionCount < cur.In.Limits.MaxActions:
		cur.ActionCount++
	default:
		cur.Locked = true
	}
	return moved(cur)
}

func stepAttemptFinished(cur Snapshot, ev Event, _ time.Time) (Decision, error) {
	if ev.Finding == nil {
		return Decision{Next: cur, JournalOnly: true}, nil
	}
	cur.Finding = *ev.Finding
	return moved(cur)
}

func stepRunFinished(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	next := cur
	switch ev.Outcome.Kind {
	case OutcomeFixed:
		return toFixed(next)
	case OutcomeVerify:
		return startVerify(next, ev.Outcome, now)
	case OutcomeAwaitDownload:
		return startDownload(next, ev.Outcome, now)
	case OutcomeEscalate:
		return toOwner(cur, next, ev.Outcome.Reason, ev.Notice)
	case OutcomeParked:
		enter(&next, Reopened)
		next.Parked = true
		bump(&next)
		return moved(next)
	}
	return Decision{Next: cur}, fmt.Errorf("lifecycle: unknown outcome %q", ev.Outcome.Kind)
}

func firstVerifyDelay(spec VerifySpec, after time.Duration, l Limits) time.Duration {
	d := min(after, l.FirstVerifyCap)
	if spec.Kind == VerifyStuckRead {
		d = max(d, l.StuckReadFloor)
	}
	return d
}

func startVerify(next Snapshot, o Outcome, now time.Time) (Decision, error) {
	enter(&next, Verifying)
	next.NextCheck = now.Add(firstVerifyDelay(o.Spec, o.After, next.In.Limits))
	spec := o.Spec
	if spec.Kind == VerifyStuckRead {
		spec.VerifyEnteredAt = now
	}
	next.Verify = &Verify{ChecksLeft: max(next.In.Limits.VerifyChecks, 1), Spec: spec}
	gen := bump(&next)
	return moved(next, check(gen, &next.Verify.Spec.CheckSeq, next.NextCheck, CheckVerify))
}

func startDownload(next Snapshot, o Outcome, now time.Time) (Decision, error) {
	if o.Target == nil {
		return Decision{Next: next}, errors.New("lifecycle: await download without a target")
	}
	d := *o.Target
	d.StartedAt, d.LastProgressAt, d.LastProgressPct = now, now, 0
	d.Stage, d.QueueStatus, d.GrabNotified = StageSearching, "", false
	enter(&next, AwaitingDownload)
	next.Download = &d
	next.NextCheck = now.Add(next.In.Limits.CheckEvery)
	gen := bump(&next)
	return moved(next, note(NoteSearching), check(gen, &next.Download.CheckSeq, next.NextCheck, CheckDownload))
}

func stepRunFinishedUnclaimed(cur Snapshot, ev Event, _ time.Time) (Decision, error) {
	if ev.Outcome.Kind != OutcomeParked {
		return Decision{Next: cur}, ErrUnclaimed
	}
	cur.Parked = true
	bump(&cur)
	return moved(cur)
}

func stepVerifyObserved(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	next := cur
	v, l := next.Verify, next.In.Limits
	if v == nil {
		return Decision{Next: cur}, errors.New("lifecycle: verifying without a verify spec")
	}
	if ev.Seq != v.Spec.CheckSeq {
		return Decision{Next: cur}, ErrStale
	}
	reschedule := func(at time.Time, effs ...Effect) (Decision, error) {
		next.NextCheck = at
		return moved(next, append(effs, check(next.Generation, &v.Spec.CheckSeq, at, CheckVerify))...)
	}
	switch ev.Verify {
	case VerifyFixed:
		return toFixed(next)
	case VerifyNotYet:
		if v.ChecksLeft > 1 {
			v.ChecksLeft--
			return reschedule(now.Add(l.VerifyGap))
		}
		if !ev.Scan || v.Spec.Extensions >= l.ExtensionsMax {
			return toOwner(cur, next, ReasonNotVerified, ev.Notice)
		}
		eta := etaMinutes(v.Spec)
		v.Spec.Extensions++
		var effs []Effect
		if !v.Spec.ETANoted {
			effs = append(effs, Effect{Kind: NotifyReporters, Note: NoteETA, ETAMinutes: eta})
		}
		v.Spec.ETANoted = true
		return reschedule(now.Add(max(l.VerifyGap, time.Duration(eta)*l.Minute)), effs...)
	case VerifyUnknown:
		if v.Spec.Extensions >= l.ExtensionsMax {
			return toOwner(cur, next, ReasonNotVerified, ev.Notice)
		}
		v.Spec.Extensions++
		return reschedule(now.Add(l.VerifyGap))
	}
	return Decision{Next: cur}, fmt.Errorf("lifecycle: unknown verify result %q", ev.Verify)
}

const defaultETAMinutes = 10

func etaMinutes(s VerifySpec) int {
	if s.ETAMinutes <= 0 {
		return defaultETAMinutes
	}
	return s.ETAMinutes
}

const planETAMinutes = 15

func stepOwnerRerun(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	next := cur
	next.Locked, next.ActionCount, next.Parked = false, 0, false
	enter(&next, Reopened)
	next.Download, next.Plan = nil, nil
	gen := bump(&next)
	if cur.In.Paused {
		next.Parked = true
		return moved(next, cancel(gen), note(NoteAnotherLook))
	}
	return moved(next, cancel(gen), launch(gen, SeedSummary, OriginOwner), note(NoteAnotherLook))
}

func stepOwnerResolve(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	next := cur
	enter(&next, Resolved)
	gen := bump(&next)
	return moved(next, cancel(gen), note(NoteResolved), Effect{Kind: SeerrResolve})
}

func stepUnlockBlocked(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	next := cur
	next.Locked, next.ActionCount = false, 0
	enter(&next, Open)
	gen := bump(&next)
	if cur.In.Paused {
		next.Parked = true
		return moved(next)
	}
	return moved(next, launch(gen, SeedFresh, OriginOwner))
}

func stepUnlock(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	if !cur.Locked {
		return Decision{Next: cur}, ErrNotLocked
	}
	cur.Locked, cur.ActionCount = false, 0
	return moved(cur)
}

func stepKeepSearching(cur Snapshot, _ Event, now time.Time) (Decision, error) {
	if cur.Reason != ReasonNoRelease || cur.Download == nil {
		return Decision{Next: cur}, ErrNoKeepSearching
	}
	next := cur
	l := next.In.Limits
	d := *cur.Download
	resetTrack(&d, now)
	d.KeepSearching, d.KeepSearchingUntil = true, now.Add(l.KeepSearchingSpan)
	enter(&next, AwaitingDownload)
	next.Download = &d
	next.NextCheck = now.Add(l.KeepSearchingCheck)
	gen := bump(&next)
	return moved(next, check(gen, &next.Download.CheckSeq, next.NextCheck, CheckDownload))
}

func resetTrack(d *Download, now time.Time) {
	d.StartedAt, d.LastProgressAt, d.LastProgressPct = now, now, 0
	d.GrabNotified, d.Stage, d.QueueStatus = false, StageSearching, ""
}

func stepPreview(cur Snapshot, ev Event, now time.Time) (Decision, error) {
	switch {
	case cur.Reason == ReasonPlanInterrupted || cur.Reason == ReasonPlanPartial:
		return Decision{Next: cur}, ErrPlanHalfRun
	case !cur.Finding.Escalatable():
		return Decision{Next: cur}, ErrNotEscalatable
	}
	cur.Plan = &Plan{JSON: append([]byte(nil), ev.Plan...), At: now, FP: ev.PlanFP}
	return moved(cur)
}

func stepSystemRerun(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	if cur.StuckReadReran {
		return Decision{Next: cur}, ErrAlreadyReran
	}
	next := cur
	next.StuckReadReran = true
	enter(&next, Reopened)
	gen := bump(&next)
	if cur.In.Paused {
		next.Parked = true
		return moved(next)
	}
	return moved(next, launch(gen, SeedSummary, OriginSystem))
}

func stepStaleRun(cur Snapshot, ev Event, _ time.Time) (Decision, error) {
	next := cur
	enter(&next, Reopened)
	gen := bump(&next)
	if cur.In.Paused {
		next.Parked = true
		return moved(next, cancel(gen))
	}
	origin := OriginRestart
	if ev.Origin == OriginStale {
		origin = OriginStale
	}
	effs := []Effect{cancel(gen), launch(gen, SeedSummary, origin)}
	if origin == OriginStale {
		effs = append(effs, owner(Notice{Kind: NoticeStale, Minutes: ev.Notice.Minutes}))
	}
	return moved(next, effs...)
}

func stepRelaunch(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	switch {
	case cur.Parked:
		return Decision{Next: cur}, ErrParked
	case cur.In.Paused:
		cur.Parked = true
		return moved(cur)
	}
	gen := bump(&cur)
	return moved(cur, launch(gen, seedFor(cur.Status), OriginRestart))
}

func stepResumed(cur Snapshot, _ Event, _ time.Time) (Decision, error) {
	switch {
	case !cur.Parked:
		return Decision{Next: cur}, ErrNotParked
	case cur.In.Paused:
		return Decision{Next: cur}, ErrPaused
	}
	cur.Parked = false
	gen := bump(&cur)
	return moved(cur, launch(gen, seedFor(cur.Status), OriginResume))
}

func stepTick(cur Snapshot, _ Event, now time.Time) (Decision, error) {
	if cur.Plan == nil || cur.PlanFresh(now) {
		return Decision{Next: cur}, ErrNoop
	}
	cur.Plan = nil
	return moved(cur)
}
