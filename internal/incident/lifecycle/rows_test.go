package lifecycle_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type K = lifecycle.EffectKind

const (
	launchK = lifecycle.LaunchRun
	cancelK = lifecycle.CancelRun
	checkK  = lifecycle.ScheduleCheck
	planK   = lifecycle.ExecutePlan
	repK    = lifecycle.NotifyReporters
	ownerK  = lifecycle.NotifyOwner
	seerrK  = lifecycle.SeerrResolve
)

type docRow struct {
	name   string
	from   lifecycle.Snapshot
	ev     tev
	to     lifecycle.State
	err    error
	gen    bool
	effs   []K
	assert func(*testing.T, lifecycle.Snapshot)
}

func ev(k lifecycle.EventKind) tev { return tev{ev: lifecycle.Event{Kind: k}} }

func evp(k lifecycle.EventKind) tev { return tev{ev: lifecycle.Event{Kind: k}, paused: true} }

func obs(o lifecycle.Observation, isLate bool) tev {
	return tev{ev: lifecycle.Event{Kind: lifecycle.DownloadObserved, Obs: &o}, late: isLate}
}

func verifyEv(r lifecycle.VerifyResult, scan bool) tev {
	return tev{ev: lifecycle.Event{Kind: lifecycle.VerifyObserved, Verify: r, Scan: scan}}
}

func runFinished(k lifecycle.OutcomeKind) tev { return tev{ev: outcome(k)} }

func noPO(t *testing.T, s lifecycle.Snapshot) {
	t.Helper()
	if s.Download != nil {
		t.Error("pending outcome kept")
	}
}

func keptPO(t *testing.T, s lifecycle.Snapshot) {
	t.Helper()
	if s.Download == nil || s.Reason != lifecycle.ReasonNoRelease {
		t.Errorf("want no_release with the outcome kept, got %s %v", s.Reason, s.Download)
	}
}

func reasonIs(r lifecycle.Reason) func(*testing.T, lifecycle.Snapshot) {
	return func(t *testing.T, s lifecycle.Snapshot) {
		t.Helper()
		if s.Reason != r {
			t.Errorf("reason %s, want %s", s.Reason, r)
		}
	}
}

func isReran(t *testing.T, s lifecycle.Snapshot) {
	t.Helper()
	if !s.StuckReadReran {
		t.Error("the rerun marker was not set")
	}
}

func isParked(t *testing.T, s lifecycle.Snapshot) {
	t.Helper()
	if !s.Parked {
		t.Error("not parked")
	}
}

func stageIs(st lifecycle.Stage) func(*testing.T, lifecycle.Snapshot) {
	return func(t *testing.T, s lifecycle.Snapshot) {
		t.Helper()
		if s.Download == nil || s.Download.Stage != st {
			t.Errorf("stage %v, want %s", s.Download, st)
		}
	}
}

func reportedRows() []docRow {
	none := lifecycle.Snapshot{ID: "inc"}
	rep := tev{ev: lifecycle.Event{Kind: lifecycle.Reported, Report: &lifecycle.Report{What: "cant_play"}}}
	repP, repS := rep, rep
	repP.paused, repS.systemic = true, true
	return []docRow{
		{name: "reported", from: none, ev: rep, to: lifecycle.Open, gen: true, effs: []K{launchK}},
		{name: "reported paused", from: none, ev: repP, to: lifecycle.Open, effs: nil, assert: isParked},
		{name: "reported systemic", from: none, ev: repS, to: lifecycle.Blocked, effs: []K{ownerK}},
		{name: "reported stored", from: snap(lifecycle.Open), ev: rep, err: lifecycle.ErrNotNew},
	}
}

func runRows() []docRow {
	inv := snap(lifecycle.Investigating)
	open := snap(lifecycle.Open)
	return []docRow{
		{name: "claim", from: open, ev: ev(lifecycle.RunClaimed), to: lifecycle.Investigating},
		{name: "claim parked", from: with(open, parked), ev: ev(lifecycle.RunClaimed), err: lifecycle.ErrParked},
		{name: "claim paused", from: open, ev: evp(lifecycle.RunClaimed), err: lifecycle.ErrPaused},
		{name: "claim retry", from: inv, ev: ev(lifecycle.RunClaimed), to: lifecycle.Investigating},
		{name: "claim stale", from: snap(lifecycle.Verifying), ev: ev(lifecycle.RunClaimed), err: lifecycle.ErrStale},
		{name: "claim old gen", from: open, ev: tev{ev: lifecycle.Event{Kind: lifecycle.RunClaimed}, stale: true},
			err: lifecycle.ErrStale},
		{name: "act paused", from: inv, ev: evp(lifecycle.ActionAttempted), err: lifecycle.ErrPaused},
		{name: "act locked", from: with(inv, locked), ev: ev(lifecycle.ActionAttempted),
			err: lifecycle.ErrDiagnosisOnly},
		{name: "act allowed", from: inv, ev: ev(lifecycle.ActionAttempted), to: lifecycle.Investigating,
			assert: func(t *testing.T, s lifecycle.Snapshot) {
				t.Helper()
				if s.ActionCount != 1 {
					t.Errorf("count %d", s.ActionCount)
				}
			}},
		{name: "act tripped", from: with(inv, spent), ev: ev(lifecycle.ActionAttempted), to: lifecycle.Investigating,
			assert: func(t *testing.T, s lifecycle.Snapshot) {
				t.Helper()
				if !s.Locked {
					t.Error("not locked")
				}
			}},
		{
			name: "act stale",
			from: snap(lifecycle.Verifying),
			ev:   ev(lifecycle.ActionAttempted),
			err:  lifecycle.ErrStale,
		},
		{name: "fixed", from: inv, ev: runFinished(lifecycle.OutcomeFixed), to: lifecycle.AgentFixed, gen: true,
			effs: []K{repK, seerrK}},
		{name: "verify", from: inv, ev: runFinished(lifecycle.OutcomeVerify), to: lifecycle.Verifying, gen: true,
			effs: []K{checkK}},
		{name: "await", from: inv, ev: runFinished(lifecycle.OutcomeAwaitDownload), to: lifecycle.AwaitingDownload,
			gen: true, effs: []K{repK, checkK}, assert: stageIs(lifecycle.StageSearching)},
		{name: "escalate", from: inv, ev: runFinished(lifecycle.OutcomeEscalate), to: lifecycle.ManualTestNeeded,
			gen: true, effs: []K{ownerK, repK}, assert: reasonIs(lifecycle.ReasonNeedsOwner)},
		{name: "parked", from: inv, ev: runFinished(lifecycle.OutcomeParked), to: lifecycle.Reopened, gen: true,
			assert: isParked},
		{name: "parked before claim", from: open, ev: runFinished(lifecycle.OutcomeParked), to: lifecycle.Open,
			gen: true, assert: isParked},
		{name: "unclaimed", from: open, ev: runFinished(lifecycle.OutcomeFixed), err: lifecycle.ErrUnclaimed},
		{name: "finished stale", from: snap(lifecycle.Resolved), ev: runFinished(lifecycle.OutcomeFixed),
			err: lifecycle.ErrStale},
	}
}

func verifyRows() []docRow {
	v := snap(lifecycle.Verifying)
	return []docRow{
		{name: "verified", from: v, ev: verifyEv(lifecycle.VerifyFixed, false), to: lifecycle.AgentFixed, gen: true,
			effs: []K{repK, seerrK}},
		{name: "not yet", from: v, ev: verifyEv(lifecycle.VerifyNotYet, false), to: lifecycle.Verifying,
			effs: []K{checkK}},
		{name: "scan running", from: with(v, lastChk), ev: verifyEv(lifecycle.VerifyNotYet, true),
			to: lifecycle.Verifying, effs: []K{repK, checkK}},
		{name: "not verified", from: with(v, lastChk), ev: verifyEv(lifecycle.VerifyNotYet, false),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{repK}, assert: reasonIs(lifecycle.ReasonNotVerified)},
		{name: "scan out of extensions", from: with(v, lastChk, extended), ev: verifyEv(lifecycle.VerifyNotYet, true),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{repK}},
		{name: "unknown", from: v, ev: verifyEv(lifecycle.VerifyUnknown, false), to: lifecycle.Verifying,
			effs: []K{checkK}},
		{name: "unknown out of extensions", from: with(v, extended), ev: verifyEv(lifecycle.VerifyUnknown, false),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{repK}},
		{name: "verify stale", from: snap(lifecycle.Open), ev: verifyEv(lifecycle.VerifyFixed, false),
			err: lifecycle.ErrStale},
	}
}

func downloadRows() []docRow {
	a := snap(lifecycle.AwaitingDownload)
	grabbed := with(a, dl(download(lifecycle.StageDownloading, true, false)))
	keep := with(a, dl(download(lifecycle.StageSearching, false, true)))
	errEv := tev{ev: lifecycle.Event{Kind: lifecycle.DownloadObserved, Err: "down"}}
	errLate := errEv
	errLate.late = true
	q := lifecycle.Observation{InQueue: true, QueueStatus: "queued", ProgressPct: 10}
	return []docRow{
		{name: "check error", from: a, ev: errEv, to: lifecycle.AwaitingDownload, effs: []K{checkK}},
		{name: "lost track", from: a, ev: errLate, to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK, repK},
			assert: noPO},
		{name: "has file", from: a, ev: obs(lifecycle.Observation{HasFile: true}, false), to: lifecycle.AgentFixed,
			gen: true, effs: []K{repK, seerrK}, assert: noPO},
		{name: "stalled", from: grabbed, ev: obs(lifecycle.Observation{InQueue: true}, true),
			to: lifecycle.ManualTestNeeded, gen: true, effs: []K{ownerK, repK}, assert: noPO},
		{name: "overlong", from: grabbed, ev: obs(q, true), to: lifecycle.ManualTestNeeded, gen: true,
			effs: []K{ownerK, repK}, assert: noPO},
		{name: "first sighting", from: a, ev: obs(q, false), to: lifecycle.AwaitingDownload, effs: []K{repK, checkK},
			assert: stageIs(lifecycle.StageDownloading)},
		{name: "progress", from: grabbed, ev: obs(q, false), to: lifecycle.AwaitingDownload, effs: []K{checkK}},
		{name: "importing", from: grabbed, ev: obs(lifecycle.Observation{}, false), to: lifecycle.AwaitingDownload,
			effs: []K{checkK}, assert: stageIs(lifecycle.StageImporting)},
		{name: "never imported", from: grabbed, ev: obs(lifecycle.Observation{}, true), to: lifecycle.ManualTestNeeded,
			gen: true, effs: []K{ownerK, repK}, assert: noPO},
		{name: "search ended", from: keep, ev: obs(lifecycle.Observation{}, true), to: lifecycle.ManualTestNeeded,
			gen: true, effs: []K{ownerK, repK}, assert: keptPO},
		{name: "keep searching", from: keep, ev: obs(lifecycle.Observation{}, false), to: lifecycle.AwaitingDownload,
			effs: []K{checkK}, assert: stageIs(lifecycle.StageSearching)},
		{name: "no release", from: a, ev: obs(lifecycle.Observation{}, true), to: lifecycle.ManualTestNeeded, gen: true,
			effs: []K{ownerK, repK}, assert: keptPO},
		{name: "within grace", from: a, ev: obs(lifecycle.Observation{}, false), to: lifecycle.AwaitingDownload,
			effs: []K{checkK}},
		{name: "download stale", from: snap(lifecycle.Verifying), ev: obs(lifecycle.Observation{HasFile: true}, false),
			err: lifecycle.ErrStale},
	}
}

func planRows() []docRow {
	x := snap(lifecycle.ExecutingPlan)
	mtn := with(snap(lifecycle.ManualTestNeeded), finding(escalatable()))
	preview := tev{ev: lifecycle.Event{Kind: lifecycle.OwnerPreview, Plan: []byte(`{}`), PlanFP: "fp1"}}
	approveLate := ev(lifecycle.OwnerApprove)
	approveLate.late = true
	return []docRow{
		{name: "plan stale", from: mtn, ev: ev(lifecycle.PlanExecuted), err: lifecycle.ErrStale},
		{name: "plan no op", from: x, ev: ev(lifecycle.PlanExecuted), err: lifecycle.ErrOpMismatch},
		{name: "preview", from: mtn, ev: preview, to: lifecycle.ManualTestNeeded},
		{name: "preview half run", from: with(mtn, reason(lifecycle.ReasonPlanInterrupted)), ev: preview,
			err: lifecycle.ErrPlanHalfRun},
		{name: "preview manual", from: snap(lifecycle.ManualTestNeeded), ev: preview, err: lifecycle.ErrNotEscalatable},
		{name: "preview open", from: snap(lifecycle.Open), ev: preview, err: lifecycle.ErrNotAwaitingDecision},
		{name: "approve no spec", from: with(mtn, planAt(base())), ev: ev(lifecycle.OwnerApprove),
			err: lifecycle.ErrNoSpec},
		{name: "approve no plan", from: mtn, ev: ev(lifecycle.OwnerApprove), err: lifecycle.ErrNoPlan},
		{name: "approve expired", from: with(mtn, planAt(base())), ev: approveLate, err: lifecycle.ErrPlanExpired},
		{name: "approve changed", from: with(mtn, planAt(base())), ev: tev{
			ev: lifecycle.Event{Kind: lifecycle.OwnerApprove}, badFP: true,
		}, err: lifecycle.ErrPlanChanged},
		{name: "approve stale version", from: with(mtn, planAt(base())), ev: tev{
			ev: lifecycle.Event{Kind: lifecycle.OwnerApprove}, badVer: true,
		}, err: lifecycle.ErrChanged},
		{name: "approve open", from: snap(lifecycle.Open), ev: ev(lifecycle.OwnerApprove),
			err: lifecycle.ErrNotAwaitingDecision},
		{name: "plan lost", from: x, ev: ev(lifecycle.PlanLost), to: lifecycle.ManualTestNeeded, gen: true,
			effs: []K{ownerK}, assert: reasonIs(lifecycle.ReasonPlanInterrupted)},
		{name: "plan lost stale", from: mtn, ev: ev(lifecycle.PlanLost), err: lifecycle.ErrStale},
		{name: "tick", from: with(mtn, planAt(base().Add(-2*time.Hour))), ev: ev(lifecycle.Tick),
			to: lifecycle.ManualTestNeeded},
		{name: "tick fresh", from: with(mtn, planAt(base())), ev: ev(lifecycle.Tick), err: lifecycle.ErrNoop},
		{name: "tick open", from: snap(lifecycle.Open), ev: ev(lifecycle.Tick), err: lifecycle.ErrNoop},
	}
}

func ownerRows() []docRow {
	mtn := snap(lifecycle.ManualTestNeeded)
	keepEv := ev(lifecycle.OwnerKeepSearching)
	keepBad := keepEv
	keepBad.badVer = true
	return []docRow{
		{name: "rerun", from: snap(lifecycle.AgentFixed), ev: ev(lifecycle.OwnerRerun), to: lifecycle.Reopened,
			gen: true, effs: []K{cancelK, launchK, repK}},
		{name: "rerun paused", from: snap(lifecycle.AgentFixed), ev: evp(lifecycle.OwnerRerun), to: lifecycle.Reopened,
			gen: true, effs: []K{cancelK, repK}, assert: isParked},
		{name: "rerun during plan", from: snap(lifecycle.ExecutingPlan), ev: ev(lifecycle.OwnerRerun),
			err: lifecycle.ErrPlanRunning},
		{name: "resolve", from: snap(lifecycle.Investigating), ev: ev(lifecycle.OwnerResolve), to: lifecycle.Resolved,
			gen: true, effs: []K{cancelK, repK, seerrK}},
		{name: "resolve resolved", from: snap(lifecycle.Resolved), ev: ev(lifecycle.OwnerResolve),
			err: lifecycle.ErrAlreadyResolved},
		{name: "unlock blocked", from: snap(lifecycle.Blocked), ev: ev(lifecycle.OwnerUnlock), to: lifecycle.Open,
			gen: true, effs: []K{launchK}},
		{name: "unlock blocked paused", from: snap(lifecycle.Blocked), ev: evp(lifecycle.OwnerUnlock),
			to: lifecycle.Open, gen: true, assert: isParked},
		{name: "unlock", from: with(mtn, locked), ev: ev(lifecycle.OwnerUnlock), to: lifecycle.ManualTestNeeded},
		{name: "unlock unlocked", from: mtn, ev: ev(lifecycle.OwnerUnlock), err: lifecycle.ErrNotLocked},
		{name: "unlock resolved", from: with(snap(lifecycle.Resolved), locked), ev: ev(lifecycle.OwnerUnlock),
			err: lifecycle.ErrResolved},
		{name: "keep", from: with(mtn, reason(lifecycle.ReasonNoRelease)), ev: keepEv, to: lifecycle.AwaitingDownload,
			gen: true, effs: []K{checkK}},
		{name: "keep stale version", from: with(mtn, reason(lifecycle.ReasonNoRelease)), ev: keepBad,
			err: lifecycle.ErrChanged},
		{name: "keep other reason", from: with(mtn, reason(lifecycle.ReasonDownloadProblem)), ev: keepEv,
			err: lifecycle.ErrNoKeepSearching},
		{name: "keep verifying", from: snap(lifecycle.Verifying), ev: keepEv, err: lifecycle.ErrNoKeepSearching},
	}
}

func systemRows() []docRow {
	inv, open := snap(lifecycle.Investigating), snap(lifecycle.Open)
	staleEv := tev{ev: lifecycle.Event{Kind: lifecycle.StaleRun, Origin: lifecycle.OriginStale}}
	return []docRow{
		{name: "system rerun", from: snap(lifecycle.AgentFixed), ev: ev(lifecycle.SystemRerun),
			to: lifecycle.Reopened, gen: true, effs: []K{launchK}, assert: isReran},
		{name: "system rerun paused", from: snap(lifecycle.AgentFixed), ev: evp(lifecycle.SystemRerun),
			to: lifecycle.Reopened, gen: true, assert: func(t *testing.T, s lifecycle.Snapshot) {
				t.Helper()
				isParked(t, s)
				isReran(t, s)
			}},
		{name: "system rerun already reran", from: with(snap(lifecycle.AgentFixed), reran),
			ev: ev(lifecycle.SystemRerun), err: lifecycle.ErrAlreadyReran},
		{name: "system rerun already reran paused", from: with(snap(lifecycle.AgentFixed), reran),
			ev: evp(lifecycle.SystemRerun), err: lifecycle.ErrAlreadyReran},
		{name: "system rerun stale", from: snap(lifecycle.ManualTestNeeded), ev: ev(lifecycle.SystemRerun),
			err: lifecycle.ErrStale},
		{name: "stale run restart", from: inv, ev: ev(lifecycle.StaleRun), to: lifecycle.Reopened, gen: true,
			effs: []K{cancelK, launchK}},
		{name: "stale run", from: inv, ev: staleEv, to: lifecycle.Reopened, gen: true,
			effs: []K{cancelK, launchK, ownerK}},
		{name: "stale run paused", from: inv, ev: evp(lifecycle.StaleRun), to: lifecycle.Reopened, gen: true,
			effs: []K{cancelK}, assert: isParked},
		{name: "stale run not running", from: open, ev: ev(lifecycle.StaleRun), err: lifecycle.ErrStale},
		{name: "relaunch", from: open, ev: ev(lifecycle.Relaunch), to: lifecycle.Open, gen: true, effs: []K{launchK}},
		{name: "relaunch paused", from: open, ev: evp(lifecycle.Relaunch), to: lifecycle.Open, assert: isParked},
		{name: "relaunch parked", from: with(open, parked), ev: ev(lifecycle.Relaunch), err: lifecycle.ErrParked},
		{name: "relaunch stale", from: snap(lifecycle.Verifying), ev: ev(lifecycle.Relaunch), err: lifecycle.ErrStale},
		{name: "resumed", from: with(open, parked), ev: ev(lifecycle.Resumed), to: lifecycle.Open, gen: true,
			effs: []K{launchK}},
		{name: "resumed paused", from: with(open, parked), ev: evp(lifecycle.Resumed), err: lifecycle.ErrPaused},
		{name: "resumed not parked", from: open, ev: ev(lifecycle.Resumed), err: lifecycle.ErrNotParked},
		{name: "resumed stale", from: snap(lifecycle.ManualTestNeeded), ev: ev(lifecycle.Resumed),
			err: lifecycle.ErrStale},
	}
}

func effectKinds(effs []lifecycle.Effect) []K {
	out := make([]K, 0, len(effs))
	for _, e := range effs {
		out = append(out, e.Kind)
	}
	slices.Sort(out)
	return out
}

func TestIncidentMachine_RowsMatchDoc(t *testing.T) {
	t.Parallel()
	all := slices.Concat(
		reportedRows(), runRows(), verifyRows(), downloadRows(), planRows(), ownerRows(), systemRows(),
		approveOpRows(), executedOpRows(), lostOpRows(), resumeOpRows(),
	)
	for _, r := range all {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			checkDocRow(t, r)
		})
	}
}

func checkDocRow(t *testing.T, r docRow) {
	t.Helper()
	d, err := apply(r.from, r.ev)
	if r.err != nil {
		if !errors.Is(err, r.err) {
			t.Fatalf("err = %v, want %v", err, r.err)
		}
		return
	}
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if d.Next.Status != r.to {
		t.Errorf("to %s, want %s", d.Next.Status, r.to)
	}
	if bumped := d.Next.Generation > r.from.Generation; bumped != r.gen && r.from.Status != lifecycle.None {
		t.Errorf("generation bumped=%v, want %v", bumped, r.gen)
	}
	want := slices.Clone(r.effs)
	slices.Sort(want)
	if got := effectKinds(d.Effects); !slices.Equal(got, want) {
		t.Errorf("effects %v, want %v", got, want)
	}
	if cerr := lifecycle.Check(d.Next); cerr != nil {
		t.Errorf("Check: %v", cerr)
	}
	if r.assert != nil {
		r.assert(t, d.Next)
	}
}
