package stuckqueue

import (
	"strings"
	"time"
)

const (
	untrackedRefusal = "no run has checked this download yet; only Remove is allowed until one does"
	inflightRefusal  = "an import for this download is running"
	waitStamp        = "2006-01-02 15:04 UTC"
)

func waiting(s Snapshot, now time.Time, minAge time.Duration) bool {
	return s.State == StateWaiting && now.Before(s.FirstSeen.Add(minAge))
}

func isTracked(s Snapshot) bool {
	return s.State != StateUntracked && s.State != StateImportInflight && s.State != StateActed
}

func alreadyDone(s Snapshot) error {
	a := s.Acted
	if a == nil {
		return refuse(changedRefusal)
	}
	return refuse("already %s at %s", strings.ToLower(actionDone(a.Action)), a.At.UTC().Format(waitStamp))
}

func Allowed(s Snapshot, a OwnerAction, now time.Time, minAge time.Duration) error {
	switch a {
	case OwnerRemove:
		return allowedRemove(s)
	case OwnerRegrab, OwnerImport:
		return allowedAct(s, now, minAge)
	case OwnerMappedImport:
		if s.State == StateActed && actedAction(s, ActionImport) {
			return nil
		}
		return allowedAct(s, now, minAge)
	case OwnerResume:
		if s.State == StateActed && unfinished(s, Event{}) {
			return nil
		}
		return refuse(changedRefusal)
	}
	return refuse(changedRefusal)
}

func allowedRemove(s Snapshot) error {
	switch s.State {
	case StateImportInflight:
		return refuse(inflightRefusal)
	case StateActed:
		return alreadyDone(s)
	case StateUntracked, StateWaiting, StateDue, StateHeld, StateNotified, StateCleared:
		return nil
	}
	return nil
}

func allowedAct(s Snapshot, now time.Time, minAge time.Duration) error {
	switch {
	case s.State == StateUntracked:
		return refuse(untrackedRefusal)
	case s.State == StateImportInflight:
		return refuse(inflightRefusal)
	case s.State == StateActed:
		return alreadyDone(s)
	case waiting(s, now, minAge):
		return refuse("this download is inside the %d h wait until %s; only Remove is allowed",
			int(minAge/time.Hour), s.FirstSeen.Add(minAge).UTC().Format(waitStamp))
	}
	return nil
}

func Actions(s Snapshot, now time.Time, minAge time.Duration) []Action {
	switch {
	case s.State == StateUntracked, waiting(s, now, minAge):
		return []Action{ActionRemove}
	case isTracked(s):
		return s.Offered.Actions()
	case s.State == StateActed && unfinished(s, Event{}):
		return []Action{Action(OwnerResume)}
	}
	return nil
}
