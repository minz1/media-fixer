package operation

import "slices"

func stepContinued(op Op, ev Event) (Op, error) {
	if ev.Version != op.Version {
		return Op{}, reject(CodeChanged, "")
	}
	if !Resumable(op) {
		return Op{}, reject(CodeNotResumable, "")
	}
	if !probesMatch(op, ev.Probes) {
		return Op{}, reject(CodeProbesStale, "")
	}
	for _, p := range ev.Probes {
		if p.Verdict == VerdictConflict {
			return Op{}, reject(CodeConflict, p.Note)
		}
	}
	for _, p := range ev.Probes {
		op.Results[p.Index] = probed(op.Steps[p.Index], op.Results[p.Index], p, ev)
	}
	op.Cause = CauseNone
	if next := firstPending(op, op.Index); next >= 0 {
		op.Status, op.Index = Running, next
		return op, nil
	}
	op.Status, op.Index = Done, len(op.Results)-1
	return op, nil
}

func probesMatch(op Op, probes []Probe) bool {
	want := Remaining(op)
	if len(probes) != len(want) {
		return false
	}
	for i, p := range probes {
		if p.Index != want[i] || !slices.Contains(Verdicts(), p.Verdict) {
			return false
		}
	}
	return true
}

func probed(st Step, old Result, p Probe, ev Event) Result {
	if p.Verdict == VerdictDone {
		old.State, old.FinishedAt, old.Err = ResultDone, ev.At, ""
		old.Note = verifiedNote(p.Note)
		return old
	}
	out := Result{State: ResultPending, Note: old.Err, QueueID: old.QueueID}
	if out.Note == "" {
		out.Note = old.Note
	}
	if st.Kind == StepQueueRemove && p.QueueID > 0 {
		out.QueueID = p.QueueID
	}
	return out
}

func verifiedNote(n string) string {
	if n == "" {
		return "verified"
	}
	return "verified: " + n
}
