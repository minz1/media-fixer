package operation_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/operation"
)

func TestOwnerContinued_AdvancesOverProbedDoneSteps(t *testing.T) {
	t.Parallel()
	op := play(readdSpec(), stopAt(0, operation.ResultUnknown)...)
	next, err := operation.Next(op, resumed(op.Version,
		operation.Probe{Index: 0, Verdict: operation.VerdictDone, Note: "no torrent with this hash"},
		operation.Probe{Index: 1, Verdict: operation.VerdictTodo}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != operation.Running || next.Index != 1 || next.Results[0].State != operation.ResultDone ||
		next.Results[0].Note == "" || next.Results[1].State != operation.ResultPending {
		t.Errorf("got %s index %d results %+v", next.Status, next.Index, next.Results)
	}
	if next.Version != op.Version+1 {
		t.Errorf("version %d, want %d", next.Version, op.Version+1)
	}
	if got := operation.Remaining(next); len(got) != 1 || got[0] != 1 {
		t.Errorf("remaining %v", got)
	}
}

func TestOwnerContinued_KeepsTheOldErrorInTheNote(t *testing.T) {
	t.Parallel()
	op := play(readdSpec(), stopAt(0, operation.ResultUnknown)...)
	next, err := operation.Next(op, resumed(op.Version, todoAll(op)...))
	if err != nil {
		t.Fatal(err)
	}
	r := next.Results[0]
	if r.Note != "boom" || r.Err != "" || !r.StartedAt.IsZero() || !r.FinishedAt.IsZero() {
		t.Errorf("result %+v", r)
	}
}

func TestOwnerContinued_CarriesProbeQueueID(t *testing.T) {
	t.Parallel()
	op := play(regrabSpec(), stopAt(0, operation.ResultUnknown)...)
	ps := todoAll(op)
	ps[0].QueueID = 4242
	next, err := operation.Next(op, resumed(op.Version, ps...))
	if err != nil {
		t.Fatal(err)
	}
	if next.Results[0].QueueID != 4242 {
		t.Errorf("queue id %d, want 4242", next.Results[0].QueueID)
	}
	if cerr := operation.Check(next); cerr != nil {
		t.Error(cerr)
	}
}

func TestInterrupted_StartedBecomesUnknown(t *testing.T) {
	t.Parallel()
	for _, c := range []operation.Cause{operation.CauseRestart, operation.CauseStore} {
		op := play(replaceSpec(1, 1), append(through(1), started(1))...)
		next, err := operation.Next(op, interrupted(c))
		if err != nil {
			t.Fatal(err)
		}
		if next.Status != operation.FailedAtStep || next.Cause != c ||
			next.Results[1].State != operation.ResultUnknown ||
			next.Results[2].State != operation.ResultPending ||
			next.Results[1].FinishedAt.IsZero() {
			t.Errorf("%s: got %s %s %+v", c, next.Status, next.Cause, next.Results)
		}
		if !operation.Resumable(next) {
			t.Errorf("%s: an interrupted started step is not resumable", c)
		}
	}
}

func TestInterrupted_RejectsErrorCause(t *testing.T) {
	t.Parallel()
	op := play(replaceSpec(1, 1), started(0))
	for _, c := range []operation.Cause{operation.CauseError, operation.CauseNone} {
		next, err := operation.Next(op, interrupted(c))
		if operation.CodeOf(err) != operation.CodeBadCause {
			t.Errorf("cause %q: err %v", c, err)
		}
		if next.Status != operation.Running {
			t.Errorf("cause %q: status %s", c, next.Status)
		}
	}
}

func TestCreated_RefusesOverMaxSteps(t *testing.T) {
	t.Parallel()
	if operation.MaxSteps() != 2000 {
		t.Fatalf("max steps %d", operation.MaxSteps())
	}
	if _, err := operation.Next(operation.Op{}, created(overSpec())); operation.CodeOf(err) != operation.CodeBadSpec {
		t.Errorf("2001 steps: %v", err)
	}
	spec := replaceSpec(1, operation.MaxSteps()-2)
	if _, err := operation.Next(operation.Op{}, created(spec)); err != nil {
		t.Errorf("2000 steps: %v", err)
	}
}

func TestNext_DoesNotMutateItsInput(t *testing.T) {
	t.Parallel()
	op := play(replaceSpec(1, 1), started(0))
	before := key(op)
	if _, err := operation.Next(op, finished(0, operation.ResultDone)); err != nil {
		t.Fatal(err)
	}
	if key(op) != before || op.Results[0].State != operation.ResultStarted {
		t.Errorf("input changed: %s", key(op))
	}
}
