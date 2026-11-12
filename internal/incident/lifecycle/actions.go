package lifecycle

import (
	"time"

	"github.com/minz1/mediafixer/internal/operation"
)

type ActionKind string

const (
	ActRerun         ActionKind = "rerun"
	ActResolve       ActionKind = "resolve"
	ActUnlock        ActionKind = "unlock"
	ActKeepSearching ActionKind = "keep"
	ActPreview       ActionKind = "preview"
	ActApprove       ActionKind = "approve"
	ActRefresh       ActionKind = "refresh"
	ActResume        ActionKind = "resume"
)

func ActionKinds() []ActionKind {
	return []ActionKind{
		ActRerun,
		ActResolve,
		ActUnlock,
		ActKeepSearching,
		ActPreview,
		ActApprove,
		ActRefresh,
		ActResume,
	}
}

type Action struct {
	Kind    ActionKind
	Version int64
}

func (k ActionKind) Event() EventKind {
	switch k {
	case ActRerun:
		return OwnerRerun
	case ActResolve:
		return OwnerResolve
	case ActUnlock:
		return OwnerUnlock
	case ActKeepSearching:
		return OwnerKeepSearching
	case ActPreview, ActRefresh:
		return OwnerPreview
	case ActApprove:
		return OwnerApprove
	case ActResume:
		return OwnerResumeOp
	}
	return ""
}

func (s Snapshot) decidable() bool {
	return s.Status == ManualTestNeeded && s.Reason != ReasonPlanInterrupted && s.Reason != ReasonPlanPartial &&
		s.Finding.Escalatable()
}

func (s Snapshot) resumable() bool {
	return s.Status == ManualTestNeeded && (s.Reason == ReasonPlanPartial || s.Reason == ReasonPlanInterrupted) &&
		s.Op != nil && operation.Resumable(*s.Op)
}

func Actions(s Snapshot, now time.Time) []Action {
	var out []Action
	add := func(k ActionKind, ok bool) {
		if ok {
			out = append(out, Action{Kind: k, Version: s.Version})
		}
	}
	fresh := s.PlanFresh(now)
	add(ActPreview, s.decidable() && !fresh)
	add(ActApprove, s.decidable() && fresh)
	add(ActRefresh, s.decidable() && fresh)
	add(ActResume, s.resumable())
	add(ActKeepSearching, s.Status == ManualTestNeeded && s.Reason == ReasonNoRelease)
	add(ActRerun, s.Status != ExecutingPlan)
	add(ActResolve, s.Status != Resolved && s.Status != ExecutingPlan)
	add(ActUnlock, s.Locked && s.Status != Resolved)
	return out
}

func Offers(s Snapshot, now time.Time, k ActionKind) bool {
	for _, a := range Actions(s, now) {
		if a.Kind == k {
			return true
		}
	}
	return false
}
