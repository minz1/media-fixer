package lifecycle

import "time"

type Observed struct {
	Snap         Snapshot
	LastActivity time.Time
}

type Liveness struct {
	Runs     map[string]int64
	Reserved map[string]bool
	Plans    map[string]bool
	Armed    map[string]int64
	Floors   map[string]time.Duration
}

type Intent struct {
	ID    string
	Event *Event
	Arm   *Effect
}

type Policy struct {
	Now        time.Time
	Paused     bool
	StaleAfter time.Duration
	Unit       time.Duration
}

type Counts struct {
	Runs      int
	Checks    int
	PlansLost int
	Parked    int
}

func Reconcile(rows []Observed, live Liveness, p Policy) []Intent {
	var out []Intent
	for _, r := range rows {
		if in, ok := reconcileOne(r, live, p); ok {
			out = append(out, in)
		}
	}
	return out
}

func reconcileOne(r Observed, live Liveness, p Policy) (Intent, bool) {
	s, now := r.Snap, p.Now
	ev := func(k EventKind) (Intent, bool) {
		return Intent{ID: s.ID, Event: &Event{Kind: k, Gen: s.Generation}}, true
	}
	runGen, running := live.Runs[s.ID]
	switch s.Status {
	case Open, Reopened:
		switch {
		case live.Reserved[s.ID]:
		case s.Parked && !p.Paused:
			return ev(Resumed)
		case !s.Parked && (!running || runGen != s.Generation):
			return ev(Relaunch)
		}
	case Investigating:
		p.StaleAfter = max(p.StaleAfter, live.Floors[s.ID])
		return reconcileRun(s, r.LastActivity, live, p)
	case Verifying, AwaitingDownload:
		if gen, armed := live.Armed[s.ID]; armed && gen == s.Generation {
			return Intent{}, false
		}
		k := CheckVerify
		if s.Status == AwaitingDownload {
			k = CheckDownload
		}
		at := s.NextCheck
		if at.Before(now) {
			at = now
		}
		eff := check(s.Generation, at, k)
		return Intent{ID: s.ID, Arm: &eff}, true
	case ExecutingPlan:
		if !live.Plans[s.ID] {
			return ev(PlanLost)
		}
	case ManualTestNeeded:
		if s.Plan != nil && !s.PlanFresh(now) {
			return Intent{ID: s.ID, Event: &Event{Kind: Tick}}, true
		}
	case AgentFixed, Blocked, Resolved, None:
	}
	return Intent{}, false
}

func reconcileRun(s Snapshot, last time.Time, live Liveness, p Policy) (Intent, bool) {
	if live.Reserved[s.ID] {
		return Intent{}, false
	}
	stale := func(o Origin, minutes int) (Intent, bool) {
		ev := &Event{Kind: StaleRun, Gen: s.Generation, Origin: o, Notice: Notice{Minutes: minutes}}
		return Intent{ID: s.ID, Event: ev}, true
	}
	gen, running := live.Runs[s.ID]
	if !running || gen != s.Generation {
		return stale(OriginRestart, 0)
	}
	unit := p.Unit
	if unit <= 0 {
		unit = time.Minute
	}
	if !last.IsZero() && p.Now.Sub(last) > p.StaleAfter {
		return stale(OriginStale, int(p.StaleAfter/unit))
	}
	return Intent{}, false
}

func Tally(intents []Intent, paused bool) Counts {
	var c Counts
	for _, in := range intents {
		switch {
		case in.Arm != nil:
			c.Checks++
		case in.Event == nil:
		case in.Event.Kind == PlanLost:
			c.PlansLost++
		case in.Event.Kind == Relaunch, in.Event.Kind == StaleRun, in.Event.Kind == Resumed:
			if paused {
				c.Parked++
			} else {
				c.Runs++
			}
		}
	}
	return c
}
