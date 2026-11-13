package operation_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/operation"
)

type docRow struct {
	name   string
	from   operation.Op
	ev     operation.Event
	to     operation.Status
	code   operation.Code
	assert func(*testing.T, operation.Op)
}

func indexIs(i int) func(*testing.T, operation.Op) {
	return func(t *testing.T, op operation.Op) {
		t.Helper()
		if op.Index != i {
			t.Errorf("index %d, want %d", op.Index, i)
		}
	}
}

func resultIs(i int, s operation.StepState) func(*testing.T, operation.Op) {
	return func(t *testing.T, op operation.Op) {
		t.Helper()
		if op.Results[i].State != s {
			t.Errorf("result %d is %s, want %s", i, op.Results[i].State, s)
		}
	}
}

func causeIs(c operation.Cause) func(*testing.T, operation.Op) {
	return func(t *testing.T, op operation.Op) {
		t.Helper()
		if op.Cause != c {
			t.Errorf("cause %q, want %q", op.Cause, c)
		}
	}
}

func all(fns ...func(*testing.T, operation.Op)) func(*testing.T, operation.Op) {
	return func(t *testing.T, op operation.Op) {
		t.Helper()
		for _, f := range fns {
			f(t, op)
		}
	}
}

func badSteps(mut func(*operation.Spec)) operation.Event {
	s := replaceSpec(1, 1)
	mut(&s)
	return created(s)
}

func createRows() []docRow {
	none := operation.Op{}
	return []docRow{
		{name: "1 replace", from: none, ev: created(replaceSpec(1, 2)), to: operation.Running,
			assert: all(indexIs(0), resultIs(0, operation.ResultPending), causeIs(operation.CauseNone))},
		{name: "1 regrab", from: none, ev: created(regrabSpec()), to: operation.Running},
		{name: "2 no steps", from: none, ev: badSteps(func(s *operation.Spec) { s.Steps = nil }),
			code: operation.CodeBadSpec},
		{name: "2 too many", from: none, ev: created(overSpec()), code: operation.CodeBadSpec},
		{name: "2 search first", from: none, ev: badSteps(func(s *operation.Spec) {
			s.Steps = []operation.Step{searchStep(), blocklistStep(1)}
		}), code: operation.CodeBadSpec},
		{name: "2 missing field", from: none, ev: badSteps(func(s *operation.Spec) { s.Steps[0].HistoryID = 0 }),
			code: operation.CodeBadSpec},
		{name: "2 no spec", from: none, ev: operation.Event{Kind: operation.Created, At: at(0)},
			code: operation.CodeBadSpec},
		{name: "2 regrab on incident", from: none, ev: badSteps(func(s *operation.Spec) {
			*s = regrabSpec()
			s.Subject.Kind = operation.SubjectIncident
		}), code: operation.CodeBadSpec},
		{name: "3 started", from: none, ev: started(0), code: operation.CodeNoOp},
		{name: "3 finished", from: none, ev: finished(0, operation.ResultDone), code: operation.CodeNoOp},
		{name: "3 interrupted", from: none, ev: interrupted(operation.CauseRestart), code: operation.CodeNoOp},
		{name: "3 continued", from: none, ev: resumed(0), code: operation.CodeNoOp},
		{name: "4 running", from: play(replaceSpec(1, 1)), ev: created(replaceSpec(1, 1)), code: operation.CodeExists},
		{name: "4 done", from: play(regrabSpec(), through(2)...), ev: created(regrabSpec()),
			code: operation.CodeExists},
		{name: "4 failed", from: play(regrabSpec(), stopAt(0, operation.ResultFailed)...), ev: created(regrabSpec()),
			code: operation.CodeExists},
	}
}

func runRows() []docRow {
	fresh := play(replaceSpec(1, 1))
	live := play(replaceSpec(1, 1), started(0))
	last := play(replaceSpec(1, 1), append(through(2), started(2))...)
	return []docRow{
		{name: "5 started", from: fresh, ev: started(0), to: operation.Running,
			assert: all(resultIs(0, operation.ResultStarted), func(t *testing.T, op operation.Op) {
				t.Helper()
				if !op.Results[0].StartedAt.Equal(at(1)) {
					t.Errorf("started at %v", op.Results[0].StartedAt)
				}
			})},
		{name: "6 skips ahead", from: fresh, ev: started(1), code: operation.CodeOutOfOrder},
		{name: "6 already started", from: live, ev: started(0), code: operation.CodeOutOfOrder},
		{name: "6 bad index", from: fresh, ev: started(-1), code: operation.CodeOutOfOrder},
		{name: "7 done then next", from: live, ev: finished(0, operation.ResultDone), to: operation.Running,
			assert: all(indexIs(1), resultIs(0, operation.ResultDone))},
		{name: "8 done last", from: last, ev: finished(2, operation.ResultDone), to: operation.Done,
			assert: all(indexIs(2), causeIs(operation.CauseNone))},
		{name: "9 failed", from: live, ev: failedWith(0, operation.ResultFailed, "404"), to: operation.FailedAtStep,
			assert: all(causeIs(operation.CauseError), resultIs(0, operation.ResultFailed), errIs(0, "404"))},
		{
			name:   "9 unknown",
			from:   live,
			ev:     failedWith(0, operation.ResultUnknown, "timeout"),
			to:     operation.FailedAtStep,
			assert: all(causeIs(operation.CauseError), resultIs(0, operation.ResultUnknown)),
		},
		{name: "10 wrong index", from: live, ev: finished(1, operation.ResultDone), code: operation.CodeOutOfOrder},
		{name: "10 not started", from: fresh, ev: finished(0, operation.ResultDone), code: operation.CodeOutOfOrder},
		{
			name: "10 pending outcome",
			from: live,
			ev:   finished(0, operation.ResultPending),
			code: operation.CodeOutOfOrder,
		},
		{
			name: "10 started outcome",
			from: live,
			ev:   finished(0, operation.ResultStarted),
			code: operation.CodeOutOfOrder,
		},
		{name: "11 restart", from: live, ev: interrupted(operation.CauseRestart), to: operation.FailedAtStep,
			assert: all(causeIs(operation.CauseRestart), resultIs(0, operation.ResultUnknown))},
		{name: "11 store pending", from: fresh, ev: interrupted(operation.CauseStore), to: operation.FailedAtStep,
			assert: all(causeIs(operation.CauseStore), resultIs(0, operation.ResultPending))},
		{name: "12 no cause", from: live, ev: interrupted(operation.CauseNone), code: operation.CodeBadCause},
		{name: "12 error cause", from: live, ev: interrupted(operation.CauseError), code: operation.CodeBadCause},
		{name: "13 owner while running", from: live, ev: resumed(0), code: operation.CodeRunning},
	}
}

func errIs(i int, want string) func(*testing.T, operation.Op) {
	return func(t *testing.T, op operation.Op) {
		t.Helper()
		if op.Results[i].Err != want {
			t.Errorf("err %q, want %q", op.Results[i].Err, want)
		}
	}
}

func finishedRows() []docRow {
	done := play(regrabSpec(), through(2)...)
	return []docRow{
		{name: "14 started", from: done, ev: started(0), code: operation.CodeFinished},
		{name: "14 finished", from: done, ev: finished(0, operation.ResultDone), code: operation.CodeFinished},
		{name: "14 interrupted", from: done, ev: interrupted(operation.CauseRestart), code: operation.CodeFinished},
		{name: "14 continued", from: done, ev: resumed(done.Version), code: operation.CodeFinished},
	}
}

func failedRows() []docRow {
	stopped := play(replaceSpec(1, 1), stopAt(1, operation.ResultUnknown)...)
	return []docRow{
		{name: "15 started", from: stopped, ev: started(1), code: operation.CodeNotRunning},
		{name: "15 finished", from: stopped, ev: finished(1, operation.ResultDone), code: operation.CodeNotRunning},
		{name: "15 interrupted", from: stopped, ev: interrupted(operation.CauseStore), code: operation.CodeNotRunning},
	}
}

func resumeRows() []docRow {
	stopped := play(replaceSpec(1, 1), stopAt(1, operation.ResultUnknown)...)
	nothing := play(replaceSpec(1, 1), stopAt(0, operation.ResultFailed)...)
	regrab := play(regrabSpec(), stopAt(0, operation.ResultUnknown)...)
	todo := todoAll
	conflicts := todo(stopped)
	conflicts[0].Verdict, conflicts[0].Note = operation.VerdictConflict, "file 1 changed since approval"
	conflicts[1].Verdict, conflicts[1].Note = operation.VerdictConflict, "second"
	doneAll := probesFor(regrab, "done")
	return []docRow{
		{name: "16 stale version", from: nothing, ev: resumed(nothing.Version+1, todo(nothing)...),
			code: operation.CodeChanged},
		{name: "17 nothing applied", from: nothing, ev: resumed(nothing.Version, todo(nothing)...),
			code: operation.CodeNotResumable},
		{name: "18 short probes", from: stopped, ev: resumed(stopped.Version, todo(stopped)[:1]...),
			code: operation.CodeProbesStale},
		{name: "18 reordered probes", from: stopped, ev: resumed(stopped.Version, reversed(todo(stopped))...),
			code: operation.CodeProbesStale},
		{name: "18 no probes", from: stopped, ev: resumed(stopped.Version), code: operation.CodeProbesStale},
		{name: "19 conflict", from: stopped, ev: resumed(stopped.Version, conflicts...), code: operation.CodeConflict},
		{name: "20 todo", from: stopped, ev: resumed(stopped.Version, todo(stopped)...), to: operation.Running,
			assert: all(indexIs(1), causeIs(operation.CauseNone), resultIs(1, operation.ResultPending))},
		{
			name: "20 mixed",
			from: regrab,
			ev: resumed(regrab.Version, operation.Probe{Index: 0, Verdict: operation.VerdictDone},
				operation.Probe{Index: 1, Verdict: operation.VerdictTodo}),
			to:     operation.Running,
			assert: all(indexIs(1), resultIs(0, operation.ResultDone), resultIs(1, operation.ResultPending)),
		},
		{name: "21 all done", from: regrab, ev: resumed(regrab.Version, doneAll...), to: operation.Done,
			assert: all(indexIs(1), causeIs(operation.CauseNone), resultIs(0, operation.ResultDone),
				resultIs(1, operation.ResultDone))},
	}
}

func reversed(ps []operation.Probe) []operation.Probe {
	out := make([]operation.Probe, 0, len(ps))
	for _, p := range slices.Backward(ps) {
		out = append(out, p)
	}
	return out
}

func TestOperationMachine_RowsMatchDoc(t *testing.T) {
	t.Parallel()
	var rows []docRow
	for _, g := range [][]docRow{createRows(), runRows(), finishedRows(), failedRows(), resumeRows()} {
		rows = append(rows, g...)
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			checkRow(t, r)
		})
	}
}

func checkRow(t *testing.T, r docRow) {
	t.Helper()
	next, err := operation.Next(r.from, r.ev)
	if r.code != "" {
		if got := operation.CodeOf(err); got != r.code {
			t.Fatalf("code %q (%v), want %q", got, err, r.code)
		}
		return
	}
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if next.Status != r.to {
		t.Errorf("to %s, want %s", next.Status, r.to)
	}
	if cerr := operation.Check(next); cerr != nil {
		t.Errorf("Check: %v", cerr)
	}
	if r.assert != nil {
		r.assert(t, next)
	}
}

func TestReject_ConflictCarriesTheFirstNote(t *testing.T) {
	t.Parallel()
	for _, r := range resumeRows() {
		if r.name != "19 conflict" {
			continue
		}
		_, err := operation.Next(r.from, r.ev)
		rej, ok := errors.AsType[*operation.RejectError](err)
		if !ok || rej.Note != "file 1 changed since approval" {
			t.Errorf("rejection %v, want the first conflict's note", err)
		}
	}
}
