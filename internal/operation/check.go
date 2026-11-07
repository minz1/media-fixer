package operation

import (
	"errors"
	"fmt"
	"slices"
)

func Check(op Op) error {
	for _, c := range []func(Op) error{checkSets, checkSize, checkProgress, checkResults, checkCounters} {
		if err := c(op); err != nil {
			return fmt.Errorf("%w: %d: %w", ErrIllegal, op.ID, err)
		}
	}
	return nil
}

func checkSets(op Op) error {
	if !slices.Contains(Statuses(), op.Status) {
		return fmt.Errorf("O1: unknown status %q", op.Status)
	}
	if op.Cause != CauseNone && !slices.Contains(Causes(), op.Cause) {
		return fmt.Errorf("O1: unknown cause %q", op.Cause)
	}
	if err := checkSubject(op.Subject, op.Kind); err != nil {
		return err
	}
	return nil
}

func checkSubject(sub Subject, kind Kind) error {
	if !slices.Contains(Kinds(), kind) {
		return fmt.Errorf("O1: unknown kind %q", kind)
	}
	if !slices.Contains(SubjectKinds(), sub.Kind) {
		return fmt.Errorf("O1: unknown subject kind %q", sub.Kind)
	}
	if sub.ID == "" {
		return errors.New("O1: empty subject id")
	}
	if (kind == KindRegrab) != (sub.Kind == SubjectDownload) {
		return fmt.Errorf("O1: %s op on a %s subject", kind, sub.Kind)
	}
	return nil
}

func checkSize(op Op) error {
	n := len(op.Steps)
	if n < 1 || n > MaxSteps() || len(op.Results) != n {
		return fmt.Errorf("O2: %d steps, %d results", n, len(op.Results))
	}
	if op.Index < 0 || op.Index >= n {
		return fmt.Errorf("O2: index %d of %d", op.Index, n)
	}
	return checkShape(op.Kind, op.Steps)
}

func checkProgress(op Op) error {
	for j := range op.Index {
		if op.Results[j].State != ResultDone {
			return fmt.Errorf("O3: step %d is %s before index %d", j, op.Results[j].State, op.Index)
		}
	}
	switch op.Status {
	case Running:
		return checkRunning(op)
	case Done:
		return checkDone(op)
	case FailedAtStep:
		return checkFailed(op)
	case None:
	}
	return nil
}

func checkRunning(op Op) error {
	if op.Cause != CauseNone {
		return fmt.Errorf("O4: running with cause %q", op.Cause)
	}
	if s := op.Results[op.Index].State; s != ResultPending && s != ResultStarted {
		return fmt.Errorf("O4: running with step %d %s", op.Index, s)
	}
	for j, r := range op.Results {
		if r.State == ResultFailed || r.State == ResultUnknown {
			return fmt.Errorf("O4: running with step %d %s", j, r.State)
		}
		if r.State == ResultStarted && j != op.Index {
			return fmt.Errorf("O4: step %d started away from index %d", j, op.Index)
		}
	}
	return nil
}

func checkDone(op Op) error {
	if op.Cause != CauseNone || op.Index != len(op.Results)-1 {
		return fmt.Errorf("O5: done with cause %q index %d", op.Cause, op.Index)
	}
	for j, r := range op.Results {
		if r.State != ResultDone {
			return fmt.Errorf("O5: done with step %d %s", j, r.State)
		}
	}
	return nil
}

func checkFailed(op Op) error {
	at := op.Results[op.Index].State
	switch op.Cause {
	case CauseError:
		if at != ResultFailed && at != ResultUnknown {
			return fmt.Errorf("O6: error cause with step %d %s", op.Index, at)
		}
	case CauseRestart, CauseStore:
		if at != ResultUnknown && at != ResultPending {
			return fmt.Errorf("O6: %s cause with step %d %s", op.Cause, op.Index, at)
		}
	case CauseNone:
		return errors.New("O6: failed without a cause")
	}
	for j := op.Index + 1; j < len(op.Results); j++ {
		if s := op.Results[j].State; s != ResultPending && s != ResultDone {
			return fmt.Errorf("O6: step %d is %s after the stop", j, s)
		}
	}
	return nil
}

func checkResults(op Op) error {
	for j, r := range op.Results {
		if err := checkResult(r, op.Steps[j].Kind); err != nil {
			return fmt.Errorf("step %d: %w", j, err)
		}
	}
	return nil
}

func checkResult(r Result, kind StepKind) error {
	if !slices.Contains(StepStates(), r.State) {
		return fmt.Errorf("O7: unknown state %q", r.State)
	}
	if r.QueueID != 0 && (r.QueueID < 0 || kind != StepQueueRemove) {
		return fmt.Errorf("O7: queue id %d on a %s step", r.QueueID, kind)
	}
	started := !r.StartedAt.IsZero()
	switch r.State {
	case ResultPending:
		if started || !r.FinishedAt.IsZero() || r.Err != "" {
			return errors.New("O7: pending with a start, finish or error")
		}
	case ResultStarted:
		if !started {
			return errors.New("O7: started without a start time")
		}
	case ResultDone, ResultFailed, ResultUnknown:
		if started && r.FinishedAt.IsZero() {
			return fmt.Errorf("O7: %s without a finish time", r.State)
		}
		if !started && r.Note == "" && r.State != ResultFailed {
			return fmt.Errorf("O7: %s without a start and without a note", r.State)
		}
	}
	return nil
}

func checkCounters(op Op) error {
	if op.Version < 0 {
		return fmt.Errorf("O9: version %d", op.Version)
	}
	return nil
}
