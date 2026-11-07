package lifecycle

import "time"

type ActionKind string

const (
	ActRerun         ActionKind = "rerun"
	ActResolve       ActionKind = "resolve"
	ActUnlock        ActionKind = "unlock"
	ActKeepSearching ActionKind = "keep"
	ActPreview       ActionKind = "preview"
	ActApprove       ActionKind = "approve"
	ActRefresh       ActionKind = "refresh"
)

func ActionKinds() []ActionKind {
	return []ActionKind{ActRerun, ActResolve, ActUnlock, ActKeepSearching, ActPreview, ActApprove, ActRefresh}
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
	}
	return ""
}

func (s Snapshot) decidable() bool {
	return s.Status == ManualTestNeeded && s.Reason != ReasonPlanInterrupted && s.Reason != ReasonPlanPartial &&
		s.Finding.Escalatable()
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
	add(ActKeepSearching, s.Status == ManualTestNeeded && s.Reason == ReasonNoRelease)
	add(ActRerun, s.Status != ExecutingPlan)
	add(ActResolve, s.Status != Resolved)
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
