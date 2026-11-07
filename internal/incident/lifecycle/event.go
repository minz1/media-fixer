package lifecycle

import (
	"errors"
	"time"
)

type EventKind string

const (
	Reported           EventKind = "reported"
	RunClaimed         EventKind = "run_claimed"
	ActionAttempted    EventKind = "action_attempted"
	AttemptFinished    EventKind = "attempt_finished"
	RunFinished        EventKind = "run_finished"
	VerifyObserved     EventKind = "verify_observed"
	DownloadObserved   EventKind = "download_observed"
	PlanExecuted       EventKind = "plan_executed"
	OwnerRerun         EventKind = "owner_rerun"
	OwnerResolve       EventKind = "owner_resolve"
	OwnerUnlock        EventKind = "owner_unlock"
	OwnerKeepSearching EventKind = "owner_keep_searching"
	OwnerPreview       EventKind = "owner_preview"
	OwnerApprove       EventKind = "owner_approve"
	SystemRerun        EventKind = "system_rerun"
	StaleRun           EventKind = "stale_run"
	Relaunch           EventKind = "relaunch"
	Resumed            EventKind = "resumed"
	PlanLost           EventKind = "plan_lost"
	Tick               EventKind = "tick"
)

func EventKinds() []EventKind {
	return []EventKind{
		Reported, RunClaimed, ActionAttempted, AttemptFinished, RunFinished, VerifyObserved, DownloadObserved,
		PlanExecuted, OwnerRerun, OwnerResolve, OwnerUnlock, OwnerKeepSearching, OwnerPreview, OwnerApprove,
		SystemRerun, StaleRun, Relaunch, Resumed, PlanLost, Tick,
	}
}

type Origin string

const (
	OriginReport  Origin = "report"
	OriginOwner   Origin = "owner"
	OriginSystem  Origin = "system"
	OriginRestart Origin = "restart"
	OriginStale   Origin = "stale"
	OriginResume  Origin = "resume"
)

type OutcomeKind string

const (
	OutcomeFixed         OutcomeKind = "fixed"
	OutcomeVerify        OutcomeKind = "verify"
	OutcomeAwaitDownload OutcomeKind = "await_download"
	OutcomeEscalate      OutcomeKind = "escalate"
	OutcomeParked        OutcomeKind = "parked"
)

type Outcome struct {
	Kind   OutcomeKind
	Spec   VerifySpec
	After  time.Duration
	Target *Download
	Reason Reason
}

type VerifyResult string

const (
	VerifyFixed   VerifyResult = "fixed"
	VerifyNotYet  VerifyResult = "not_yet"
	VerifyUnknown VerifyResult = "unknown"
)

type Observation struct {
	HasFile     bool
	InQueue     bool
	QueueStatus string
	ProgressPct float64
}

type Partial struct {
	Applied bool
	Line    string
}

type Report struct {
	What   string
	ItemID string
}

type Event struct {
	Kind       EventKind
	Gen        int64
	Version    int64
	HasVersion bool
	Origin     Origin
	Tool       string
	Outcome    Outcome
	Exit       string
	Finding    *Finding
	Verify     VerifyResult
	Scan       bool
	Obs        *Observation
	Err        string
	Plan       []byte
	PlanFP     string
	Partial    Partial
	Report     *Report
	Notice     Notice
}

var (
	ErrStale               = errors.New("stale")
	ErrChanged             = errors.New("this changed, look again")
	ErrParked              = errors.New("parked")
	ErrPaused              = errors.New("autonomous actions paused")
	ErrDiagnosisOnly       = errors.New("budget spent, diagnosis only")
	ErrNotNew              = errors.New("not a new incident")
	ErrUnclaimed           = errors.New("the run has not claimed the incident")
	ErrPlanRunning         = errors.New("the approved plan is running")
	ErrAlreadyResolved     = errors.New("incident already resolved")
	ErrNotLocked           = errors.New("this incident is not locked")
	ErrResolved            = errors.New("this incident is resolved")
	ErrNoKeepSearching     = errors.New("this incident is not awaiting a keep-searching decision")
	ErrPlanHalfRun         = errors.New("the approved plan already ran partly; rerun or resolve it")
	ErrNotEscalatable      = errors.New("this incident has no escalation action recommended")
	ErrNotAwaitingDecision = errors.New("this incident is not waiting for an escalation decision")
	ErrNoPlan              = errors.New("preview this escalation before approving it, so the plan " +
		"that runs is the one you saw")
	ErrPlanExpired = errors.New("may no longer describe the same files; preview it again")
	ErrPlanChanged = errors.New("the plan changed since you previewed it; preview it again")
	ErrNotParked   = errors.New("not parked")
	ErrNoop        = errors.New("nothing to do")
)

func workerEvent(k EventKind) bool {
	switch k {
	case RunClaimed, ActionAttempted, AttemptFinished, RunFinished, VerifyObserved, DownloadObserved,
		PlanExecuted, SystemRerun, StaleRun, Relaunch, Resumed, PlanLost:
		return true
	case Reported, OwnerRerun, OwnerResolve, OwnerUnlock, OwnerKeepSearching, OwnerPreview, OwnerApprove, Tick:
	}
	return false
}

func versionChecked(k EventKind) bool {
	switch k {
	case OwnerApprove, OwnerPreview, OwnerKeepSearching:
		return true
	case OwnerRerun, OwnerResolve, OwnerUnlock, Reported, RunClaimed, ActionAttempted, AttemptFinished,
		RunFinished, VerifyObserved, DownloadObserved, PlanExecuted, SystemRerun, StaleRun, Relaunch, Resumed,
		PlanLost, Tick:
	}
	return false
}
