package stuckqueue

import (
	"fmt"
	"slices"
	"time"
)

type guard func(Snapshot, Event) bool

type step func(Snapshot, Event) (Snapshot, []Effect, error)

type row struct {
	id    string
	from  []State
	kinds []EventKind
	guard guard
	step  step
}

func (r row) matches(s Snapshot, e Event) bool {
	return slices.Contains(r.from, s.State) && slices.Contains(r.kinds, e.Kind) && (r.guard == nil || r.guard(s, e))
}

func tracked() []State {
	return []State{StateWaiting, StateDue, StateHeld, StateNotified, StateCleared}
}

func anyState() []State {
	return slices.Concat([]State{StateUntracked, StateImportInflight, StateActed}, tracked())
}

func only(st ...State) []State { return st }

func kinds(k ...EventKind) []EventKind { return k }

func all(gs ...guard) guard {
	return func(s Snapshot, e Event) bool {
		for _, g := range gs {
			if !g(s, e) {
				return false
			}
		}
		return true
	}
}

func not(g guard) guard { return func(s Snapshot, e Event) bool { return !g(s, e) } }

func inWait(s Snapshot, e Event) bool {
	return s.State == StateWaiting && e.At.Before(s.FirstSeen.Add(e.MinAge))
}

func hasOp(_ Snapshot, e Event) bool { return e.OpID > 0 }

func noOp(_ Snapshot, e Event) bool { return e.OpID <= 0 }

func hasCommand(_ Snapshot, e Event) bool { return e.CommandID > 0 }

func noCommand(_ Snapshot, e Event) bool { return e.CommandID <= 0 }

func doneEv(_ Snapshot, e Event) bool { return e.Done }

func notDoneEv(_ Snapshot, e Event) bool { return !e.Done }

func eventAction(a Action) guard {
	return func(_ Snapshot, e Event) bool { return e.Action == a }
}

func notRemoveAction(_ Snapshot, e Event) bool {
	return e.Kind != EvActed || e.Action == ActionRegrab || e.Action == ActionImport
}

func actedOp(s Snapshot) int64 {
	if s.Acted == nil {
		return 0
	}
	return s.Acted.OpID
}

func actedAction(s Snapshot, a Action) bool { return s.Acted != nil && s.Acted.Action == a }

func unfinished(s Snapshot, _ Event) bool {
	return s.Acted != nil && s.Acted.Result == ResultUnfinished
}

func sameOp(s Snapshot, e Event) bool { return e.OpID == actedOp(s) }

func otherOp(s Snapshot, e Event) bool { return e.OpID != actedOp(s) }

func graceOver(s Snapshot, e Event) bool {
	return s.Acted != nil && e.At.Sub(s.Acted.At) >= Grace
}

func graceLeft(s Snapshot, e Event) bool { return !graceOver(s, e) }

func plainActed(s Snapshot, _ Event) bool {
	return actedAction(s, ActionRemove) || (actedAction(s, ActionRegrab) && s.Acted.Result == ResultOK)
}

func importedAll(s Snapshot, _ Event) bool {
	return actedAction(s, ActionImport) && s.Acted.Files.N >= s.Acted.Files.M
}

func importedSome(s Snapshot, _ Event) bool {
	return actedAction(s, ActionImport) && s.Acted.Files.N < s.Acted.Files.M
}

func mappedRest(s Snapshot, e Event) bool {
	return actedAction(s, ActionImport) && e.Mapped && e.CommandID > 0
}

func restWithoutCommand(s Snapshot, e Event) bool {
	return e.Kind == EvActed && e.Mapped && e.Action == ActionImport && actedAction(s, ActionImport)
}

func timedOut(s Snapshot, e Event) bool {
	return s.Import != nil && e.At.Sub(s.Import.StartedAt) >= e.Timeout
}

func cmdIs(c ...CmdStatus) guard {
	return func(_ Snapshot, e Event) bool { return slices.Contains(c, e.Cmd) }
}

func expired(s Snapshot, e Event) bool {
	return s.Acted != nil && e.At.Sub(s.Acted.At) >= Keep
}

func missesLeft(s Snapshot, _ Event) bool { return s.Misses < 1 }

func rejecting(c Code) step {
	return func(s Snapshot, _ Event) (Snapshot, []Effect, error) { return s, nil, reject(c, "") }
}

func rejectingOp(c Code) step {
	return func(s Snapshot, e Event) (Snapshot, []Effect, error) {
		return s, nil, reject(c, fmt.Sprintf("op %d", e.OpID))
	}
}

func unchanged(s Snapshot, _ Event) (Snapshot, []Effect, error) { return s, nil, nil }

func transitions() []row {
	return slices.Concat(rowsUntracked(), rowsRun(), rowsOwner(), rowsInflight(), rowsActed(), rowsOps())
}

func rowsUntracked() []row {
	untracked := only(StateUntracked)
	return []row{
		{"1", untracked, kinds(EvSeen), nil, stepFirstSeen},
		{"2", untracked, kinds(EvActed), eventAction(ActionRemove), stepActedRemoveNew},
		{
			"3", untracked, kinds(
				EvMissed, EvClaimed, EvNotifyFailed, EvClearDone, EvClearFailed, EvActed, EvImportPosted,
				EvCommandObserved, EvExpire,
			),
			notRemoveAction, rejecting(CodeUntracked),
		},
	}
}

func rowsRun() []row {
	return []row{
		{"4", tracked(), kinds(EvSeen), nil, stepSeen},
		{"5", tracked(), kinds(EvMissed), missesLeft, stepMissed},
		{"6", tracked(), kinds(EvMissed), not(missesLeft), stepPrune},
		{"7", only(StateDue), kinds(EvClaimed), nil, stepClaimed},
		{
			"8", only(StateWaiting, StateHeld, StateNotified, StateCleared), kinds(EvClaimed), nil,
			rejecting(CodeNotDue),
		},
		{"9", only(StateNotified), kinds(EvNotifyFailed), claimMatches, stepNotifyFailed},
		{"10", only(StateNotified), kinds(EvNotifyFailed), not(claimMatches), rejecting(CodeStaleClaim)},
		{
			"11", only(StateWaiting, StateDue, StateHeld, StateCleared), kinds(EvNotifyFailed), nil,
			rejecting(CodeNotNotified),
		},
		{"12", only(StateHeld), kinds(EvClearDone), nil, stepClearDone},
		{"13", only(StateHeld), kinds(EvClearFailed), nil, unchanged},
		{
			"14", only(StateWaiting, StateDue, StateNotified, StateCleared), kinds(EvClearDone, EvClearFailed), nil,
			rejecting(CodeNotClearing),
		},
	}
}

func claimMatches(s Snapshot, e Event) bool { return s.NotifiedAt.Equal(e.ClaimAt) }

func rowsOwner() []row {
	open := not(inWait)
	return []row{
		{"15", tracked(), kinds(EvActed), eventAction(ActionRemove), stepActedRemove},
		{"16", tracked(), kinds(EvActed), all(open, eventAction(ActionRegrab), hasOp), stepActedRegrab},
		{"16a", tracked(), kinds(EvActed), all(open, eventAction(ActionRegrab), noOp), rejecting(CodeNoOp)},
		{"17", tracked(), kinds(EvActed), all(open, eventAction(ActionImport)), stepActedImport},
		{
			"18", only(StateWaiting), kinds(EvActed, EvImportPosted), all(inWait, notRemoveAction),
			rejecting(CodeInsideWait),
		},
		{"19", tracked(), kinds(EvImportPosted), all(open, hasCommand), stepImportPosted},
		{"20", tracked(), kinds(EvImportPosted), all(open, noCommand), rejecting(CodeNoCommand)},
		{"21", tracked(), kinds(EvCommandObserved), nil, rejecting(CodeNoImport)},
		{"22", tracked(), kinds(EvExpire), nil, rejecting(CodeNotActed)},
	}
}

func rowsInflight() []row {
	inflight := only(StateImportInflight)
	return []row{
		{"23", inflight, kinds(EvSeen), nil, stepInflightSeen},
		{"24", inflight, kinds(EvMissed), nil, unchanged},
		{"25", inflight, kinds(EvCommandObserved), cmdIs(CmdCompleted), stepImportLanded},
		{"25a", inflight, kinds(EvCommandObserved), cmdIs(CmdGone), noted(ResultImportUnconfirmed)},
		{"26", inflight, kinds(EvCommandObserved), cmdIs(CmdFailed), noted(ResultImportFailed)},
		{
			"27", inflight, kinds(EvCommandObserved), all(cmdIs(CmdRunning), timedOut),
			noted(ResultImportTimeout),
		},
		{
			"28", inflight, kinds(EvCommandObserved), all(cmdIs(CmdUnreadable), timedOut),
			noted(ResultImportUnconfirmed),
		},
		{
			"29", inflight, kinds(EvCommandObserved), all(cmdIs(CmdRunning, CmdUnreadable), not(timedOut)),
			unchanged,
		},
		{
			"30", inflight, kinds(
				EvClaimed, EvNotifyFailed, EvClearDone, EvClearFailed, EvActed, EvImportPosted, EvExpire,
			),
			nil, rejecting(CodeImportRunning),
		},
	}
}

func rowsActed() []row {
	acted := only(StateActed)
	return []row{
		{"31", acted, kinds(EvSeen), graceLeft, stepActedSeen},
		{"32", acted, kinds(EvSeen), all(graceOver, plainActed), stepActedDropped},
		{"33", acted, kinds(EvSeen), all(graceOver, importedAll), stepActedReturns(ResultImportFailed)},
		{"34", acted, kinds(EvSeen), all(graceOver, importedSome), stepActedReturns(ResultPartial)},
		{"35", acted, kinds(EvMissed), nil, unchanged},
		{"36", acted, kinds(EvExpire), expired, stepPrune},
		{"37", acted, kinds(EvExpire), not(expired), rejecting(CodeNotExpired)},
		{"38", acted, kinds(EvImportPosted), mappedRest, stepImportPosted},
		{"38a", acted, kinds(EvActed), restWithoutCommand, stepActedImport},
		{
			"39", acted, kinds(EvClaimed, EvNotifyFailed, EvClearDone, EvClearFailed, EvActed, EvCommandObserved),
			not(restWithoutCommand), rejecting(CodeAlreadyDone),
		},
		{"39", acted, kinds(EvImportPosted), not(mappedRest), rejecting(CodeAlreadyDone)},
		{"40", acted, kinds(EvSeen), all(graceOver, unfinished), stepUnfinishedSeen},
	}
}

func rowsOps() []row {
	positive := func(g guard) guard { return all(hasOp, g) }
	return []row{
		{"41", only(StateUntracked), kinds(EvAdopted), hasOp, stepAdoptedNew},
		{"42", tracked(), kinds(EvAdopted), hasOp, stepAdopted},
		{"43", only(StateActed), kinds(EvAdopted), positive(sameOp), rejecting(CodeAdopted)},
		{"44", only(StateActed), kinds(EvAdopted), positive(otherOp), rejectingOp(CodeAlreadyDone)},
		{"45", only(StateImportInflight), kinds(EvAdopted), hasOp, rejectingOp(CodeImportRunning)},
		{"46", only(StateActed), kinds(EvResumed), all(unfinished, hasOp, sameOp, doneEv), stepResumed(ResultOK)},
		{
			"47", only(StateActed), kinds(EvResumed), all(unfinished, hasOp, sameOp, notDoneEv),
			stepResumed(ResultUnfinished),
		},
		{"48", only(StateActed), kinds(EvResumed), all(unfinished, hasOp, otherOp), rejecting(CodeOpChanged)},
		{"49", only(StateActed), kinds(EvResumed), all(not(unfinished), hasOp), rejecting(CodeNotResumable)},
		{
			"50", slices.Concat(only(StateUntracked, StateImportInflight), tracked()), kinds(EvResumed), hasOp,
			rejecting(CodeNotResumable),
		},
		{"51", anyState(), kinds(EvAdopted, EvResumed), noOp, rejecting(CodeNoOp)},
	}
}

func Matches(s Snapshot, e Event) int {
	n := 0
	for _, r := range transitions() {
		if r.matches(s, e) {
			n++
		}
	}
	return n
}

func RowID(s Snapshot, e Event) string {
	for _, r := range transitions() {
		if r.matches(s, e) {
			return r.id
		}
	}
	return ""
}

func Next(s Snapshot, e Event) (Snapshot, []Effect, error) {
	for _, r := range transitions() {
		if !r.matches(s, e) {
			continue
		}
		next, effs, err := r.step(s, e)
		if err != nil {
			return s, nil, err
		}
		return next, effs, nil
	}
	return s, nil, fmt.Errorf("stuckqueue: no row for %q x %q", s.State, e.Kind)
}

func refresh(s Snapshot) []Effect { return []Effect{EffRefreshCard{Key: s.Key}} }

func stamp(s Snapshot, at time.Time) time.Time {
	if at.Before(s.FirstSeen) {
		return s.FirstSeen
	}
	return at
}

func coalesce(s Snapshot, at time.Time) time.Time {
	if s.NotifiedAt.IsZero() {
		return stamp(s, at)
	}
	return s.NotifiedAt
}

func gone(s Snapshot) Snapshot { return Snapshot{Key: s.Key, State: StateUntracked} }

func settle(cur Snapshot, e Event) (Snapshot, []Effect) {
	next := cur
	next.Misses, next.TargetsFP, next.Import, next.Offered = 0, e.Obs.FP, nil, e.Obs.Offered
	switch {
	case e.At.Before(cur.FirstSeen.Add(e.MinAge)):
		next.State, next.Offered, next.Acted = StateWaiting, OfferRemove, nil
		return next, nil
	case e.Obs.Verdict == VerdictClear:
		next.State, next.Offered, next.Acted = StateHeld, OfferRemove, nil
		if e.Hold {
			return next, nil
		}
		return next, []Effect{EffClearDownload{Key: cur.Key}}
	case cur.NotifiedAt.IsZero():
		next.State, next.Acted = StateDue, nil
	default:
		next.State = StateNotified
	}
	return next, nil
}

func cardChanged(a, b Snapshot) bool {
	if a.State != b.State || a.Offered != b.Offered || a.TargetsFP != b.TargetsFP {
		return true
	}
	if a.Acted == nil || b.Acted == nil {
		return a.Acted != b.Acted
	}
	return *a.Acted != *b.Acted
}

func stepFirstSeen(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.FirstSeen = e.At
	next, effs := settle(cur, e)
	return next, effs, nil
}

func stepSeen(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	next, effs := settle(cur, e)
	if cardChanged(cur, next) {
		effs = append(effs, refresh(next)...)
	}
	return next, effs, nil
}

func stepMissed(cur Snapshot, _ Event) (Snapshot, []Effect, error) {
	cur.Misses = 1
	return cur, nil, nil
}

func stepPrune(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	if e.Kind == EvExpire {
		return gone(cur), nil, nil
	}
	return gone(cur), refresh(cur), nil
}

func stepClaimed(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.State, cur.NotifiedAt = StateNotified, stamp(cur, e.At)
	return cur, nil, nil
}

func stepNotifyFailed(cur Snapshot, _ Event) (Snapshot, []Effect, error) {
	cur.State, cur.NotifiedAt, cur.Acted = StateDue, time.Time{}, nil
	return cur, nil, nil
}

func stepClearDone(cur Snapshot, _ Event) (Snapshot, []Effect, error) {
	cur.State = StateCleared
	return cur, nil, nil
}

func toActed(cur Snapshot, a Acted) Snapshot {
	cur.State, cur.Misses, cur.Offered, cur.Import, cur.Acted = StateActed, 0, OfferNone, nil, &a
	return cur
}

func stepActedRemove(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	next := toActed(cur, Acted{Action: ActionRemove, At: stamp(cur, e.At), Result: ResultOK})
	return next, refresh(next), nil
}

func stepActedRemoveNew(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.FirstSeen = e.At
	return stepActedRemove(cur, e)
}

func stepActedRegrab(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	res := ResultOK
	if e.Resumable {
		res = ResultUnfinished
	}
	next := toActed(cur, Acted{Action: ActionRegrab, At: stamp(cur, e.At), Result: res, OpID: e.OpID})
	return next, refresh(next), nil
}

func stepActedImport(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	next := toActed(cur, Acted{Action: ActionImport, At: stamp(cur, e.At), Result: ResultOK, Files: e.Files})
	return next, refresh(next), nil
}

func stepImportPosted(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.State, cur.Misses, cur.Offered, cur.Acted = StateImportInflight, 0, OfferNone, nil
	cur.Import = &ImportCmd{CommandID: e.CommandID, StartedAt: stamp(cur, e.At), Files: e.Files}
	return cur, refresh(cur), nil
}

func stepInflightSeen(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.Misses, cur.TargetsFP = 0, e.Obs.FP
	return cur, nil, nil
}

func stepImportLanded(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	next := toActed(cur, Acted{Action: ActionImport, At: stamp(cur, e.At), Result: ResultOK, Files: cur.Import.Files})
	return next, refresh(next), nil
}

func noted(res ActResult) step {
	return func(cur Snapshot, e Event) (Snapshot, []Effect, error) {
		at := stamp(cur, e.At)
		next := cur
		next.State, next.Misses, next.Offered, next.NotifiedAt = StateNotified, 0, OfferRemove, coalesce(cur, e.At)
		next.Acted = &Acted{Action: ActionImport, At: at, Result: res, Files: cur.Import.Files}
		next.Import = nil
		return next, refresh(next), nil
	}
}

func stepActedSeen(cur Snapshot, _ Event) (Snapshot, []Effect, error) {
	cur.Misses = 0
	return cur, nil, nil
}

func stepActedDropped(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.NotifiedAt, cur.Acted = coalesce(cur, e.At), nil
	next, effs := settle(cur, e)
	return next, append(effs, refresh(next)...), nil
}

func stepActedReturns(res ActResult) step {
	return func(cur Snapshot, e Event) (Snapshot, []Effect, error) {
		kept := *cur.Acted
		kept.Result = res
		settled, _ := settle(cur, e)
		settled.State, settled.NotifiedAt, settled.Acted = StateNotified, coalesce(cur, e.At), &kept
		return settled, refresh(settled), nil
	}
}

func stepUnfinishedSeen(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.Misses, cur.TargetsFP = 0, e.Obs.FP
	return cur, nil, nil
}

func stepAdopted(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	next := toActed(cur, Acted{Action: ActionRegrab, At: stamp(cur, e.At), Result: ResultUnfinished, OpID: e.OpID})
	return next, refresh(next), nil
}

func stepAdoptedNew(cur Snapshot, e Event) (Snapshot, []Effect, error) {
	cur.FirstSeen = e.At
	return stepAdopted(cur, e)
}

func stepResumed(res ActResult) step {
	return func(cur Snapshot, e Event) (Snapshot, []Effect, error) {
		next := toActed(cur, Acted{Action: ActionRegrab, At: stamp(cur, e.At), Result: res, OpID: e.OpID})
		next.Misses = 0
		return next, refresh(next), nil
	}
}
