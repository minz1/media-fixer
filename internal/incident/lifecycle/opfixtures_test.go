package lifecycle_test

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

const opID int64 = 5

func planSource() json.RawMessage { return json.RawMessage(`{"files":[]}`) }

func opSpec() operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: "inc"},
		Kind:    operation.KindTorrentReadd,
		Title:   "Show S01",
		Source:  planSource(),
		Steps: []operation.Step{
			{Kind: operation.StepTorrentDelete, InfoHash: "abc"},
			{Kind: operation.StepTorrentAdd, InfoHash: "abc", Magnet: "magnet:?xt=urn:btih:abc"},
		},
	}
}

func begin(i int) operation.Event { return operation.Event{Kind: operation.StepStarted, Index: i} }

func finish(i int, out operation.StepState) operation.Event {
	e := operation.Event{Kind: operation.StepFinished, Index: i, Outcome: out}
	if out == operation.ResultFailed || out == operation.ResultUnknown {
		e.Err = "boom"
	}
	return e
}

func interrupt(c operation.Cause) operation.Event {
	return operation.Event{Kind: operation.Interrupted, Cause: c}
}

func mustNext(op operation.Op, e operation.Event) operation.Op {
	e.At = base()
	next, err := operation.Next(op, e)
	if err != nil {
		panic(fmt.Sprintf("fixture: %v", err))
	}
	return next
}

func buildOp(evs ...operation.Event) operation.Op {
	spec := opSpec()
	op := mustNext(operation.Op{}, operation.Event{Kind: operation.Created, Spec: &spec})
	for _, e := range evs {
		op = mustNext(op, e)
	}
	op.ID = opID
	return op
}

func ready(op operation.Op) bool {
	return op.Status == operation.Running && op.Results[op.Index].State == operation.ResultPending
}

func advanced(op operation.Op, mode string) operation.Op {
	switch mode {
	case "started":
		if ready(op) {
			return mustNext(op, begin(op.Index))
		}
	case "failed":
		if ready(op) {
			op = mustNext(op, begin(op.Index))
			return mustNext(op, finish(op.Index, operation.ResultFailed))
		}
	case "done":
		for ready(op) {
			op = mustNext(op, begin(op.Index))
			op = mustNext(op, finish(op.Index, operation.ResultDone))
		}
	}
	return op
}

func setOp(op operation.Op) func(*lifecycle.Snapshot) {
	return func(s *lifecycle.Snapshot) {
		o := op
		o.Results = append([]operation.Result(nil), op.Results...)
		s.Op = &o
	}
}

func execOp(op operation.Op) lifecycle.Snapshot {
	return with(snap(lifecycle.ExecutingPlan), setOp(op))
}

func mtnOp(r lifecycle.Reason, op operation.Op) lifecycle.Snapshot {
	return with(snap(lifecycle.ManualTestNeeded), finding(escalatable()), reason(r), setOp(op))
}

func runningOp() operation.Op { return buildOp() }

func startedOp() operation.Op { return buildOp(begin(0)) }

func doneOp() operation.Op {
	return buildOp(begin(0), finish(0, operation.ResultDone), begin(1), finish(1, operation.ResultDone))
}

func failedFirstOp() operation.Op { return buildOp(begin(0), finish(0, operation.ResultFailed)) }

func failedSecondOp() operation.Op {
	return buildOp(begin(0), finish(0, operation.ResultDone), begin(1), finish(1, operation.ResultFailed))
}

func restartNothingOp() operation.Op { return buildOp(interrupt(operation.CauseRestart)) }

func restartAppliedOp() operation.Op { return buildOp(begin(0), interrupt(operation.CauseRestart)) }

func storeAppliedOp() operation.Op {
	return buildOp(begin(0), finish(0, operation.ResultDone), interrupt(operation.CauseStore))
}

func execSamples() []lifecycle.Snapshot {
	var out []lifecycle.Snapshot
	for _, op := range []operation.Op{
		runningOp(), startedOp(), doneOp(), failedFirstOp(), failedSecondOp(), restartNothingOp(),
		restartAppliedOp(), storeAppliedOp(),
	} {
		out = append(out, execOp(op))
	}
	return out
}

func mtnSamples() []lifecycle.Snapshot {
	return []lifecycle.Snapshot{
		mtnOp(lifecycle.ReasonPlanFailed, failedFirstOp()),
		mtnOp(lifecycle.ReasonPlanFailed, restartNothingOp()),
		mtnOp(lifecycle.ReasonPlanPartial, failedSecondOp()),
		mtnOp(lifecycle.ReasonPlanInterrupted, restartAppliedOp()),
		mtnOp(lifecycle.ReasonPlanInterrupted, storeAppliedOp()),
	}
}

func opKey(s lifecycle.Snapshot) string {
	o := s.Op
	if o == nil {
		return ""
	}
	var rs []string
	for _, r := range o.Results {
		rs = append(rs, string(r.State))
	}
	return fmt.Sprintf("|op%d/%s/%s/%d/%s", o.ID, o.Status, o.Cause, o.Index, strings.Join(rs, ","))
}

func resumeFor(cur lifecycle.Snapshot, e lifecycle.Event, t tev) lifecycle.Event {
	if cur.Op == nil {
		return e
	}
	e.OpID, e.OpVersion = cur.Op.ID, cur.Op.Version
	if t.badOp {
		e.OpVersion++
	}
	verdict := operation.VerdictTodo
	if t.verdict != "" {
		verdict = t.verdict
	}
	for _, i := range operation.Remaining(*cur.Op) {
		e.Probes = append(e.Probes, operation.Probe{Index: i, Verdict: verdict, Note: "probe note"})
	}
	return e
}

func inPlanStates(s lifecycle.Snapshot) bool {
	if s.Status == lifecycle.ExecutingPlan {
		return true
	}
	return s.Status == lifecycle.ManualTestNeeded && (s.Reason == lifecycle.ReasonPlanFailed ||
		s.Reason == lifecycle.ReasonPlanPartial || s.Reason == lifecycle.ReasonPlanInterrupted)
}
