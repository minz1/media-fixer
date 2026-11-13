package operation_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/operation"
)

func opWith(states ...operation.StepState) operation.Op {
	op := operation.Op{Status: operation.FailedAtStep}
	for _, s := range states {
		op.Results = append(op.Results, operation.Result{State: s})
	}
	return op
}

func TestApplied_AnyStartedDoneOrUnknown(t *testing.T) {
	t.Parallel()
	for _, s := range []operation.StepState{operation.ResultStarted, operation.ResultDone, operation.ResultUnknown} {
		if !operation.Applied(opWith(operation.ResultPending, s)) {
			t.Errorf("a %s result is not counted as applied", s)
		}
	}
}

func TestApplied_DefinitiveFailureIsNotApplied(t *testing.T) {
	t.Parallel()
	if operation.Applied(opWith(operation.ResultFailed, operation.ResultPending)) {
		t.Error("a definitive failure with nothing before it counts as applied")
	}
	if !operation.Applied(opWith(operation.ResultDone, operation.ResultFailed)) {
		t.Error("a failure after a done step does not count as applied")
	}
}

func TestApplied_NothingStartedIsNotApplied(t *testing.T) {
	t.Parallel()
	op := play(replaceSpec(1, 1), interrupted(operation.CauseRestart))
	if operation.Applied(op) {
		t.Error("an interrupted op with nothing started counts as applied")
	}
	if operation.Resumable(op) {
		t.Error("an op with nothing applied is resumable")
	}
}

func TestResumable_RegrabRemovalUnknownIsResumable(t *testing.T) {
	t.Parallel()
	op := play(regrabSpec(), stopAt(0, operation.ResultUnknown)...)
	if !operation.Resumable(op) {
		t.Error("a regrab with the removal unknown is not resumable")
	}
}

func TestResumable_RegrabRemovalFailedIsNot(t *testing.T) {
	t.Parallel()
	op := play(regrabSpec(), stopAt(0, operation.ResultFailed)...)
	if operation.Resumable(op) {
		t.Error("a regrab with the removal failed is resumable")
	}
	if operation.Resumable(play(replaceSpec(1, 1))) {
		t.Error("a running op is resumable")
	}
}

func TestRemaining_SkipsDoneFromIndex(t *testing.T) {
	t.Parallel()
	op := play(replaceSpec(1, 2), stopAt(1, operation.ResultFailed)...)
	got := operation.Remaining(op)
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("remaining %v, want [1 2 3]", got)
	}
}

func TestLine_ReplacePartialCounts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   operation.Op
		want string
	}{
		{
			"failed after two of three deletes",
			play(replaceSpec(1, 3), stopAt(3, operation.ResultFailed)...),
			"Stopped at step 4 of 5: blocklisted 1 of 1, deleted 2 of 3 files, search not sent.",
		},
		{
			"unknown delete after a restart",
			play(replaceSpec(1, 2), append(through(2), started(2), interrupted(operation.CauseRestart))...),
			"Interrupted by a restart at step 3 of 4: blocklisted 1 of 1, deleted 1 of 2 files, 1 may be deleted, " +
				"search not sent.",
		},
		{
			"store cause",
			play(replaceSpec(0, 2), append(through(1), started(1), interrupted(operation.CauseStore))...),
			"Stopped by a database error at step 2 of 3: deleted 1 of 2 files, 1 may be deleted, search not sent.",
		},
		{
			"nothing changed",
			play(replaceSpec(1, 1), interrupted(operation.CauseRestart)),
			"Interrupted by a restart before step 1 of 3; nothing changed.",
		},
		{
			"search unknown",
			play(replaceSpec(1, 1), stopAt(2, operation.ResultUnknown)...),
			"Stopped at step 3 of 3: blocklisted 1 of 1, deleted 1 of 1 file, the search may have been sent.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := operation.Line(c.op); got != c.want {
				t.Errorf("line %q, want %q", got, c.want)
			}
		})
	}
}

func TestLine_RegrabRemovalUnknown(t *testing.T) {
	t.Parallel()
	op := play(regrabSpec(), stopAt(0, operation.ResultUnknown)...)
	if got, want := operation.Line(op), "The removal may have landed; search not sent."; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
	removed := play(regrabSpec(), stopAt(1, operation.ResultFailed)...)
	if got, want := operation.Line(removed), "Removed; search not sent."; got != want {
		t.Errorf("line %q, want %q", got, want)
	}
}

func TestNextLine_NamesWhatResumeDoes(t *testing.T) {
	t.Parallel()
	op := play(replaceSpec(1, 2), stopAt(2, operation.ResultFailed)...)
	if got, want := operation.NextLine(op), "Resume: delete 1 file, send SeasonSearch."; got != want {
		t.Errorf("next %q, want %q", got, want)
	}
	if got := operation.NextLine(play(replaceSpec(1, 1))); got != "" {
		t.Errorf("a running op says %q", got)
	}
}
