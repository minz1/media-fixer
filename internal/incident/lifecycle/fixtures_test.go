package lifecycle_test

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/operation"
)

func base() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

const late = 60 * 24 * time.Hour

func limits() lifecycle.Limits {
	return lifecycle.Limits{
		MaxActions: 3, SystemicThreshold: 5, VerifyChecks: 2, ExtensionsMax: 2, Minute: time.Minute,
		FirstVerifyCap: 30 * time.Minute, VerifyGap: 2 * time.Minute, StuckReadFloor: 3 * time.Minute,
		ApprovedVerify: 10 * time.Minute, CheckEvery: 5 * time.Minute, NoReleaseGrace: 15 * time.Minute,
		Stall: time.Hour, TrackCap: 12 * time.Hour, KeepSearchingCheck: time.Hour,
		KeepSearchingSpan: 7 * 24 * time.Hour, PlanTTL: time.Hour,
	}
}

func escalatable() lifecycle.Finding {
	return lifecycle.Finding{
		EscalateAction: lifecycle.EscalateRemoveAndSearch,
		JSON:           json.RawMessage(`{"escalate_action":"remove_and_search"}`),
	}
}

func manual() lifecycle.Finding {
	return lifecycle.Finding{
		EscalateAction: "manual_investigation",
		JSON:           json.RawMessage(`{"escalate_action":"manual_investigation"}`),
	}
}

func target() *lifecycle.Download {
	return &lifecycle.Download{MediaType: "series", Title: "Show", Season: 1, Episode: 2}
}

func download(stage lifecycle.Stage, grabbed, keep bool) *lifecycle.Download {
	d := target()
	d.StartedAt, d.LastProgressAt, d.Stage, d.GrabNotified, d.KeepSearching = base(), base(), stage, grabbed, keep
	if keep {
		d.KeepSearchingUntil = base().Add(7 * 24 * time.Hour)
	}
	if stage == lifecycle.StageDownloading {
		d.QueueStatus = "downloading"
	}
	return d
}

func snap(s lifecycle.State) lifecycle.Snapshot {
	out := lifecycle.Snapshot{ID: "inc", Status: s, Generation: 3, Version: 7, In: lifecycle.Inputs{Limits: limits()}}
	switch s {
	case lifecycle.Blocked:
		out.Locked = true
	case lifecycle.ManualTestNeeded:
		out.Reason = lifecycle.ReasonNeedsOwner
	case lifecycle.Verifying:
		out.NextCheck = base()
		out.Verify = &lifecycle.Verify{ChecksLeft: 2, Spec: lifecycle.VerifySpec{Kind: lifecycle.VerifyGeneric}}
	case lifecycle.AwaitingDownload:
		out.NextCheck = base()
		out.Download = download(lifecycle.StageSearching, false, false)
	case lifecycle.ExecutingPlan:
		out.Finding = escalatable()
		out.Plan = &lifecycle.Plan{JSON: []byte(`{"files":[]}`), At: base(), FP: "fp1"}
	case lifecycle.Open, lifecycle.Reopened, lifecycle.Investigating, lifecycle.AgentFixed, lifecycle.Resolved,
		lifecycle.None:
	}
	return out
}

func with(s lifecycle.Snapshot, fns ...func(*lifecycle.Snapshot)) lifecycle.Snapshot {
	s = s.Clone()
	for _, f := range fns {
		f(&s)
	}
	return s
}

func locked(s *lifecycle.Snapshot)  { s.Locked, s.ActionCount = true, 3 }
func parked(s *lifecycle.Snapshot)  { s.Parked = true }
func paused(s *lifecycle.Snapshot)  { s.In.Paused = true }
func spent(s *lifecycle.Snapshot)   { s.ActionCount = 3 }
func lastChk(s *lifecycle.Snapshot) { s.Verify.ChecksLeft = 1 }
func extended(s *lifecycle.Snapshot) {
	s.Verify.Spec.Extensions = limits().ExtensionsMax
}

func reason(r lifecycle.Reason) func(*lifecycle.Snapshot) {
	return func(s *lifecycle.Snapshot) {
		s.Reason = r
		if r == lifecycle.ReasonNoRelease {
			s.Download = download(lifecycle.StageSearching, false, false)
		}
	}
}

func finding(f lifecycle.Finding) func(*lifecycle.Snapshot) {
	return func(s *lifecycle.Snapshot) { s.Finding = f }
}

func planAt(at time.Time) func(*lifecycle.Snapshot) {
	return func(s *lifecycle.Snapshot) { s.Plan = &lifecycle.Plan{JSON: []byte(`{"files":[]}`), At: at, FP: "fp1"} }
}

func dl(d *lifecycle.Download) func(*lifecycle.Snapshot) {
	return func(s *lifecycle.Snapshot) { s.Download = d }
}

func samples(s lifecycle.State) []lifecycle.Snapshot {
	b := snap(s)
	out := []lifecycle.Snapshot{b, with(b, locked), with(b, paused)}
	switch s {
	case lifecycle.Open, lifecycle.Reopened:
		out = append(out, with(b, parked), with(b, parked, paused))
	case lifecycle.Investigating:
		out = append(out, with(b, spent), with(b, locked, paused))
	case lifecycle.Verifying:
		out = append(out, with(b, lastChk), with(b, lastChk, extended))
	case lifecycle.AwaitingDownload:
		out = append(out,
			with(b, dl(download(lifecycle.StageDownloading, true, false))),
			with(b, dl(download(lifecycle.StageImporting, true, false))),
			with(b, dl(download(lifecycle.StageSearching, false, true))))
	case lifecycle.ManualTestNeeded:
		for _, r := range lifecycle.Reasons() {
			out = append(out, with(b, reason(r)), with(b, reason(r), finding(escalatable())))
		}
		out = append(out,
			with(b, finding(escalatable()), planAt(base())),
			with(b, finding(escalatable()), planAt(base().Add(-2*time.Hour))),
			with(b, reason(lifecycle.ReasonNoRelease), finding(escalatable()), planAt(base())))
	case lifecycle.ExecutingPlan:
		out = append(out, execSamples()...)
	case lifecycle.AgentFixed, lifecycle.Blocked, lifecycle.Resolved, lifecycle.None:
	}
	return out
}

type tev struct {
	ev        lifecycle.Event
	stale     bool
	badVer    bool
	badFP     bool
	paused    bool
	late      bool
	systemic  bool
	opRunning bool
	badOp     bool
	verdict   operation.Verdict
	advance   string
}

func (t tev) String() string {
	return fmt.Sprintf("%s stale=%v badVer=%v badFP=%v paused=%v late=%v systemic=%v err=%q op=%v/%v/%s/%s",
		t.ev.Kind, t.stale, t.badVer, t.badFP, t.paused, t.late, t.systemic, t.ev.Err, t.opRunning, t.badOp,
		t.verdict, t.advance)
}

func nowFor(t tev) time.Time {
	if t.late {
		return base().Add(late)
	}
	return base()
}

func prepare(s lifecycle.Snapshot, t tev) (lifecycle.Snapshot, lifecycle.Event) {
	cur := s
	cur.In.Limits = limits()
	cur.In.Paused = t.paused
	cur.In.ActiveCount = 0
	if t.systemic {
		cur.In.ActiveCount = limits().SystemicThreshold
	}
	cur.In.OpRunning = t.opRunning
	if cur.Op != nil && t.advance != "" {
		op := advanced(*cur.Op, t.advance)
		cur.Op = &op
	}
	ev := t.ev
	ev.Gen = s.Generation
	if t.stale {
		ev.Gen = s.Generation + 1
	}
	ev.Version, ev.HasVersion = s.Version, true
	if t.badVer {
		ev.Version++
	}
	if ev.Kind == lifecycle.OwnerApprove && s.Plan != nil && !t.badFP {
		ev.PlanFP = s.Plan.FP
	}
	if ev.Kind == lifecycle.OwnerResumeOp {
		ev = resumeFor(cur, ev, t)
	}
	return cur, ev
}

func apply(s lifecycle.Snapshot, t tev) (lifecycle.Decision, error) {
	cur, ev := prepare(s, t)
	d, err := lifecycle.Next(cur, ev, nowFor(t))
	if err != nil {
		return d, err
	}
	d.Next = lifecycle.Versioned(cur, d.Next)
	return d, nil
}

func outcome(k lifecycle.OutcomeKind) lifecycle.Event {
	o := lifecycle.Outcome{Kind: k}
	switch k {
	case lifecycle.OutcomeVerify:
		o.Spec, o.After = lifecycle.VerifySpec{Kind: lifecycle.VerifyGeneric}, time.Minute
	case lifecycle.OutcomeAwaitDownload:
		o.Target = target()
	case lifecycle.OutcomeEscalate:
		o.Reason = lifecycle.ReasonNeedsOwner
	case lifecycle.OutcomeFixed, lifecycle.OutcomeParked:
	}
	return lifecycle.Event{
		Kind:    lifecycle.RunFinished,
		Outcome: o,
		Notice:  lifecycle.Notice{Kind: lifecycle.NoticeCard},
	}
}

func payloads(k lifecycle.EventKind) []lifecycle.Event {
	e := lifecycle.Event{Kind: k}
	esc, man := escalatable(), manual()
	switch k {
	case lifecycle.RunFinished:
		return []lifecycle.Event{
			outcome(lifecycle.OutcomeFixed), outcome(lifecycle.OutcomeVerify), outcome(lifecycle.OutcomeAwaitDownload),
			outcome(lifecycle.OutcomeEscalate), outcome(lifecycle.OutcomeParked),
		}
	case lifecycle.AttemptFinished:
		return []lifecycle.Event{e, {Kind: k, Finding: &esc}, {Kind: k, Finding: &man}}
	case lifecycle.VerifyObserved:
		return []lifecycle.Event{
			{Kind: k, Verify: lifecycle.VerifyFixed}, {Kind: k, Verify: lifecycle.VerifyNotYet},
			{Kind: k, Verify: lifecycle.VerifyNotYet, Scan: true}, {Kind: k, Verify: lifecycle.VerifyUnknown},
		}
	case lifecycle.DownloadObserved:
		return []lifecycle.Event{
			{Kind: k, Err: "arr down"}, {Kind: k, Obs: &lifecycle.Observation{HasFile: true}},
			{Kind: k, Obs: &lifecycle.Observation{InQueue: true, QueueStatus: "queued", ProgressPct: 10}},
			{Kind: k, Obs: &lifecycle.Observation{}},
		}
	case lifecycle.PlanExecuted:
		return []lifecycle.Event{
			e,
			{Kind: k, Err: "boom"},
			{Kind: k, Err: "boom", Partial: lifecycle.Partial{Applied: true}},
			{Kind: k, OpID: opID},
			{Kind: k, OpID: opID + 1},
		}
	case lifecycle.PlanLost:
		return []lifecycle.Event{
			e, {Kind: k, Cause: operation.CauseRestart}, {Kind: k, Cause: operation.CauseStore},
			{Kind: k, Cause: operation.CauseError},
		}
	case lifecycle.OwnerApprove:
		spec := opSpec()
		return []lifecycle.Event{e, {Kind: k, Spec: &spec}, {Kind: k, BuildErr: "premise changed"}}
	case lifecycle.OwnerResumeOp:
		return []lifecycle.Event{{Kind: k, PlanFP: "fp1"}}
	case lifecycle.StaleRun:
		return []lifecycle.Event{{Kind: k, Origin: lifecycle.OriginRestart}, {Kind: k, Origin: lifecycle.OriginStale}}
	case lifecycle.OwnerPreview:
		return []lifecycle.Event{{Kind: k, Plan: []byte(`{"files":[]}`), PlanFP: "fp1"}}
	case lifecycle.Reported:
		return []lifecycle.Event{{Kind: k, Report: &lifecycle.Report{What: "cant_play"}}}
	case lifecycle.RunClaimed, lifecycle.ActionAttempted, lifecycle.OwnerRerun, lifecycle.OwnerResolve,
		lifecycle.OwnerUnlock, lifecycle.OwnerKeepSearching, lifecycle.SystemRerun, lifecycle.Relaunch,
		lifecycle.Resumed, lifecycle.Tick:
	}
	return []lifecycle.Event{e}
}

func pausable(k lifecycle.EventKind) bool {
	switch k {
	case lifecycle.Reported, lifecycle.RunClaimed, lifecycle.ActionAttempted, lifecycle.OwnerRerun,
		lifecycle.OwnerUnlock, lifecycle.SystemRerun, lifecycle.StaleRun, lifecycle.Relaunch, lifecycle.Resumed:
		return true
	case lifecycle.AttemptFinished, lifecycle.RunFinished, lifecycle.VerifyObserved, lifecycle.DownloadObserved,
		lifecycle.PlanExecuted, lifecycle.OwnerResolve, lifecycle.OwnerKeepSearching, lifecycle.OwnerPreview,
		lifecycle.OwnerApprove, lifecycle.PlanLost, lifecycle.OwnerResumeOp, lifecycle.Tick:
	}
	return false
}

func timed(k lifecycle.EventKind) bool {
	return k == lifecycle.DownloadObserved || k == lifecycle.OwnerApprove || k == lifecycle.Tick
}

func events(k lifecycle.EventKind) []tev {
	var out []tev
	for _, p := range payloads(k) {
		out = append(out, tev{ev: p})
		if pausable(k) {
			out = append(out, tev{ev: p, paused: true})
		}
		if timed(k) {
			out = append(out, tev{ev: p, late: true})
		}
		if k == lifecycle.PlanExecuted || k == lifecycle.PlanLost {
			for _, mode := range []string{"started", "failed", "done"} {
				out = append(out, tev{ev: p, advance: mode})
			}
		}
	}
	first := payloads(k)[0]
	out = append(out, tev{ev: first, stale: true}, tev{ev: first, badVer: true})
	return append(out, extraEvents(k)...)
}

func extraEvents(k lifecycle.EventKind) []tev {
	ps := payloads(k)
	switch k {
	case lifecycle.OwnerApprove:
		return []tev{{ev: ps[0], badFP: true}, {ev: ps[1], opRunning: true}}
	case lifecycle.Reported:
		return []tev{{ev: ps[0], systemic: true}}
	case lifecycle.OwnerResumeOp:
		return []tev{
			{ev: ps[0], verdict: operation.VerdictDone}, {ev: ps[0], verdict: operation.VerdictConflict},
			{ev: ps[0], badOp: true},
		}
	case lifecycle.RunClaimed, lifecycle.ActionAttempted, lifecycle.AttemptFinished, lifecycle.RunFinished,
		lifecycle.VerifyObserved, lifecycle.DownloadObserved, lifecycle.PlanExecuted, lifecycle.OwnerRerun,
		lifecycle.OwnerResolve, lifecycle.OwnerUnlock, lifecycle.OwnerKeepSearching, lifecycle.OwnerPreview,
		lifecycle.SystemRerun, lifecycle.StaleRun, lifecycle.Relaunch, lifecycle.Resumed, lifecycle.PlanLost,
		lifecycle.Tick:
	}
	return nil
}

func states() []lifecycle.State {
	return append([]lifecycle.State{lifecycle.None}, lifecycle.States()...)
}

func key(s lifecycle.Snapshot) string {
	k := fmt.Sprintf("%s|l%v|n%d|p%v|r%s|f%s|plan%v", s.Status, s.Locked, s.ActionCount, s.Parked, s.Reason,
		s.Finding.EscalateAction, s.Plan != nil)
	if v := s.Verify; v != nil {
		k += fmt.Sprintf("|v%d/%d/%v/%s", v.ChecksLeft, v.Spec.Extensions, v.Spec.ETANoted, v.Spec.Kind)
	}
	if d := s.Download; d != nil {
		k += fmt.Sprintf("|d%s/%v/%v/%s", d.Stage, d.GrabNotified, d.KeepSearching, d.QueueStatus)
	}
	k += opKey(s)
	return k
}

func incidentMachine() machinetest.Machine[lifecycle.State, lifecycle.EventKind, lifecycle.Snapshot, tev] {
	initial := lifecycle.Snapshot{ID: "inc", In: lifecycle.Inputs{Limits: limits()}}
	return machinetest.Machine[lifecycle.State, lifecycle.EventKind, lifecycle.Snapshot, tev]{
		States:  states(),
		Kinds:   lifecycle.EventKinds(),
		Initial: initial,
		State:   func(s lifecycle.Snapshot) lifecycle.State { return s.Status },
		Key:     key,
		Samples: func(s lifecycle.State) []lifecycle.Snapshot {
			if s == lifecycle.None {
				return []lifecycle.Snapshot{initial}
			}
			return samples(s)
		},
		Events:  events,
		Matches: func(s lifecycle.Snapshot, t tev) int { return lifecycle.Matches(s.Status, t.ev.Kind) },
		Next: func(s lifecycle.Snapshot, t tev) (lifecycle.Snapshot, error) {
			d, err := apply(s, t)
			if err != nil {
				return s, err
			}
			return d.Next, nil
		},
		Check:    lifecycle.Check,
		Terminal: func(s lifecycle.State) bool { return s == lifecycle.Resolved },
		Extra:    obligations,
	}
}
