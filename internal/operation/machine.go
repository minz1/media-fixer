package operation

import (
	"fmt"
	"slices"
	"time"
)

type step func(op Op, ev Event) (Op, error)

type row struct {
	from  []Status
	kinds []EventKind
	step  step
}

func rejectWith(c Code) step {
	return func(Op, Event) (Op, error) { return Op{}, reject(c, "") }
}

func transitions() []row {
	live := []Status{Running, Done, FailedAtStep}
	return []row{
		{[]Status{None}, []EventKind{Created}, stepCreated},
		{[]Status{None}, []EventKind{StepStarted, StepFinished, Interrupted, OwnerContinued}, rejectWith(CodeNoOp)},
		{live, []EventKind{Created}, rejectWith(CodeExists)},
		{[]Status{Running}, []EventKind{StepStarted}, stepStarted},
		{[]Status{Running}, []EventKind{StepFinished}, stepFinished},
		{[]Status{Running}, []EventKind{Interrupted}, stepInterrupted},
		{[]Status{Running}, []EventKind{OwnerContinued}, rejectWith(CodeRunning)},
		{
			[]Status{Done}, []EventKind{StepStarted, StepFinished, Interrupted, OwnerContinued},
			rejectWith(CodeFinished),
		},
		{[]Status{FailedAtStep}, []EventKind{StepStarted, StepFinished, Interrupted}, rejectWith(CodeNotRunning)},
		{[]Status{FailedAtStep}, []EventKind{OwnerContinued}, stepContinued},
	}
}

func Matches(s Status, k EventKind) int {
	n := 0
	for _, r := range transitions() {
		if slices.Contains(r.from, s) && slices.Contains(r.kinds, k) {
			n++
		}
	}
	return n
}

func Next(op Op, ev Event) (Op, error) {
	for _, r := range transitions() {
		if !slices.Contains(r.from, op.Status) || !slices.Contains(r.kinds, ev.Kind) {
			continue
		}
		next, err := r.step(cloneOp(op), ev)
		if err != nil {
			return op, err
		}
		if ev.Kind != Created {
			next.Version = op.Version + 1
		}
		next.UpdatedAt = ev.At
		return next, nil
	}
	return op, fmt.Errorf("operation: no row for %q x %q", op.Status, ev.Kind)
}

func cloneOp(op Op) Op {
	op.Results = slices.Clone(op.Results)
	return op
}

func stepCreated(_ Op, ev Event) (Op, error) {
	if ev.Spec == nil {
		return Op{}, reject(CodeBadSpec, "no spec")
	}
	spec := *ev.Spec
	if err := validateSpec(spec.Subject, spec.Kind, spec.Steps); err != nil {
		return Op{}, reject(CodeBadSpec, err.Error())
	}
	results := make([]Result, len(spec.Steps))
	for i := range results {
		results[i].State = ResultPending
	}
	return Op{
		Subject: spec.Subject, Kind: spec.Kind, Title: spec.Title, Source: slices.Clone(spec.Source),
		Steps: slices.Clone(spec.Steps), Results: results, Status: Running, CreatedAt: ev.At,
	}, nil
}

func stepStarted(op Op, ev Event) (Op, error) {
	if ev.Index != op.Index || ev.Index < 0 || ev.Index >= len(op.Results) ||
		op.Results[ev.Index].State != ResultPending {
		return Op{}, reject(CodeOutOfOrder, "")
	}
	r := &op.Results[ev.Index]
	r.State, r.StartedAt, r.FinishedAt = ResultStarted, ev.At, time.Time{}
	return op, nil
}

func stepFinished(op Op, ev Event) (Op, error) {
	i := ev.Index
	if i != op.Index || i < 0 || i >= len(op.Results) || op.Results[i].State != ResultStarted {
		return Op{}, reject(CodeOutOfOrder, "")
	}
	r := &op.Results[i]
	r.Note, r.Err, r.FinishedAt = ev.Note, ev.Err, ev.At
	switch ev.Outcome {
	case ResultDone:
		r.State = ResultDone
		return advance(op), nil
	case ResultFailed, ResultUnknown:
		r.State = ev.Outcome
		op.Status, op.Cause = FailedAtStep, CauseError
		return op, nil
	case ResultPending, ResultStarted:
	}
	return Op{}, reject(CodeOutOfOrder, "")
}

func advance(op Op) Op {
	if next := firstPending(op, op.Index+1); next >= 0 {
		op.Index = next
		return op
	}
	op.Status, op.Index = Done, len(op.Results)-1
	return op
}

func firstPending(op Op, from int) int {
	for j := from; j < len(op.Results); j++ {
		if op.Results[j].State == ResultPending {
			return j
		}
	}
	return -1
}

func stepInterrupted(op Op, ev Event) (Op, error) {
	if ev.Cause != CauseRestart && ev.Cause != CauseStore {
		return Op{}, reject(CodeBadCause, string(ev.Cause))
	}
	op.Status, op.Cause = FailedAtStep, ev.Cause
	if r := &op.Results[op.Index]; r.State == ResultStarted {
		r.State, r.FinishedAt = ResultUnknown, ev.At
		r.Note, r.Err = interruptNote(ev), ev.Err
	}
	return op, nil
}

func interruptNote(ev Event) string {
	if ev.Note != "" {
		return ev.Note
	}
	if ev.Cause == CauseStore {
		return "a database write failed while the step ran"
	}
	return "the service restarted while the step ran"
}
