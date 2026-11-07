package operation_test

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/operation"
)

type tev struct {
	ev     operation.Event
	probes string
	badVer bool
}

func base() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

func at(n int) time.Time { return base().Add(time.Duration(n) * time.Minute) }

func blocklistStep(n int) operation.Step {
	return operation.Step{
		Kind: operation.StepBlocklist, App: "sonarr", HistoryID: n, DownloadID: fmt.Sprintf("dl%d", n),
		GrabbedAt: base(), MediaID: 5,
	}
}

func deleteStep(n int) operation.Step {
	return operation.Step{
		Kind: operation.StepDeleteFile, App: "sonarr", FileID: n, Path: fmt.Sprintf("/tv/e%d.mkv", n), Size: 100,
		MediaID: 5,
	}
}

func searchStep() operation.Step {
	return operation.Step{Kind: operation.StepSearch, App: "sonarr", Command: "SeasonSearch", MediaID: 5, Season: 1}
}

func replaceSpec(blocks, deletes int) operation.Spec {
	var steps []operation.Step
	for i := 1; i <= blocks; i++ {
		steps = append(steps, blocklistStep(i))
	}
	for i := 1; i <= deletes; i++ {
		steps = append(steps, deleteStep(i))
	}
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: "inc-1"}, Kind: operation.KindReplace,
		Title: "Show S01", Source: []byte(`{"plan":1}`), Steps: append(steps, searchStep()),
	}
}

func readdSpec() operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: "inc-1"}, Kind: operation.KindTorrentReadd,
		Title: "Show S01",
		Steps: []operation.Step{
			{Kind: operation.StepTorrentDelete, InfoHash: "abc", Category: "tv", Name: "Show"},
			{Kind: operation.StepTorrentAdd, InfoHash: "abc", Category: "tv", Magnet: "magnet:?xt=urn:btih:abc"},
		},
	}
}

func regrabSpec() operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectDownload, ID: "dl-1"}, Kind: operation.KindRegrab,
		Title: "Show S01E02",
		Steps: []operation.Step{
			{Kind: operation.StepQueueRemove, App: "sonarr", QueueID: 9, DownloadID: "dl-1", MediaID: 5},
			searchStep(),
		},
	}
}

func created(spec operation.Spec) operation.Event {
	return operation.Event{Kind: operation.Created, At: at(0), Spec: &spec}
}

func started(i int) operation.Event {
	return operation.Event{Kind: operation.StepStarted, At: at(1), Index: i}
}

func finished(i int, s operation.StepState) operation.Event {
	return operation.Event{Kind: operation.StepFinished, At: at(2), Index: i, Outcome: s}
}

func failedWith(i int, s operation.StepState, errText string) operation.Event {
	e := finished(i, s)
	e.Err = errText
	return e
}

func interrupted(c operation.Cause) operation.Event {
	return operation.Event{Kind: operation.Interrupted, At: at(3), Cause: c}
}

func resumed(v int64, probes ...operation.Probe) operation.Event {
	return operation.Event{Kind: operation.OwnerContinued, At: at(4), Version: v, Probes: probes}
}

func play(spec operation.Spec, evs ...operation.Event) operation.Op {
	op, err := operation.Next(operation.Op{}, created(spec))
	if err != nil {
		panic(err)
	}
	for _, e := range evs {
		if op, err = operation.Next(op, e); err != nil {
			panic(fmt.Sprintf("%+v: %v", e, err))
		}
	}
	return op
}

func through(n int) []operation.Event {
	var out []operation.Event
	for i := range n {
		out = append(out, started(i), finished(i, operation.ResultDone))
	}
	return out
}

func stopAt(n int, outcome operation.StepState) []operation.Event {
	return append(through(n), started(n), failedWith(n, outcome, "boom"))
}

func todoAll(op operation.Op) []operation.Probe {
	var out []operation.Probe
	for _, i := range operation.Remaining(op) {
		out = append(out, operation.Probe{Index: i, Verdict: operation.VerdictTodo})
	}
	return out
}

func probesFor(op operation.Op, mode string) []operation.Probe {
	ps := todoAll(op)
	switch mode {
	case "done":
		for i := range ps {
			ps[i].Verdict = operation.VerdictDone
		}
	case "mixed":
		if len(ps) > 0 {
			ps[0].Verdict = operation.VerdictDone
		}
	case "conflict":
		if len(ps) > 0 {
			ps[len(ps)-1].Verdict, ps[len(ps)-1].Note = operation.VerdictConflict, "file changed"
		}
	case "stale":
		return append(ps, operation.Probe{Index: 99, Verdict: operation.VerdictTodo})
	case "qid":
		for i := range ps {
			ps[i].QueueID = 77
		}
	}
	return ps
}

func prepare(op operation.Op, t tev) operation.Event {
	ev := t.ev
	if ev.Kind != operation.OwnerContinued {
		return ev
	}
	ev.Version = op.Version
	if t.badVer {
		ev.Version++
	}
	ev.Probes = probesFor(op, t.probes)
	return ev
}

func apply(op operation.Op, t tev) (operation.Op, error) { return operation.Next(op, prepare(op, t)) }

func samples(s operation.Status) []operation.Op {
	switch s {
	case operation.None:
		return []operation.Op{{}}
	case operation.Running:
		return runningSamples()
	case operation.Done:
		return []operation.Op{play(replaceSpec(1, 1), through(3)...), play(readdSpec(), through(2)...),
			play(regrabSpec(), through(2)...)}
	case operation.FailedAtStep:
		return failedSamples()
	}
	return nil
}

func runningSamples() []operation.Op {
	return []operation.Op{
		play(replaceSpec(1, 1)),
		play(replaceSpec(1, 1), started(0)),
		play(replaceSpec(1, 1), through(1)...),
		play(replaceSpec(1, 1), append(through(1), started(1))...),
		play(replaceSpec(1, 1), append(through(2), started(2))...),
		play(readdSpec()),
		play(regrabSpec(), started(0)),
	}
}

func failedSamples() []operation.Op {
	return []operation.Op{
		play(replaceSpec(1, 1), stopAt(1, operation.ResultFailed)...),
		play(replaceSpec(1, 1), stopAt(1, operation.ResultUnknown)...),
		play(replaceSpec(1, 1), stopAt(0, operation.ResultFailed)...),
		play(replaceSpec(1, 1), append(through(1), started(1), interrupted(operation.CauseRestart))...),
		play(replaceSpec(1, 1), interrupted(operation.CauseRestart)),
		play(replaceSpec(1, 1), append(through(2), interrupted(operation.CauseStore))...),
		play(readdSpec(), stopAt(0, operation.ResultUnknown)...),
		play(readdSpec(), stopAt(1, operation.ResultFailed)...),
		play(regrabSpec(), stopAt(0, operation.ResultUnknown)...),
		play(regrabSpec(), stopAt(0, operation.ResultFailed)...),
		play(regrabSpec(), stopAt(1, operation.ResultUnknown)...),
	}
}

func overSpec() operation.Spec { return replaceSpec(operation.MaxSteps(), 0) }

func eventsFor(k operation.EventKind) []tev {
	var out []tev
	switch k {
	case operation.Created:
		bad := replaceSpec(1, 1)
		bad.Steps = nil
		for _, s := range []operation.Spec{replaceSpec(1, 1), readdSpec(), regrabSpec(), bad, overSpec()} {
			out = append(out, tev{ev: created(s)})
		}
	case operation.StepStarted:
		for _, i := range []int{-1, 0, 1, 2, 9} {
			out = append(out, tev{ev: started(i)})
		}
	case operation.StepFinished:
		for _, i := range []int{0, 1, 2, 9} {
			for _, s := range []operation.StepState{operation.ResultDone, operation.ResultFailed, operation.ResultUnknown} {
				out = append(out, tev{ev: failedWith(i, s, "boom")})
			}
		}
		out = append(out, tev{ev: finished(0, operation.ResultPending)}, tev{ev: finished(0, operation.ResultStarted)})
	case operation.Interrupted:
		for _, c := range []operation.Cause{operation.CauseNone, operation.CauseError, operation.CauseRestart,
			operation.CauseStore} {
			out = append(out, tev{ev: interrupted(c)})
		}
	case operation.OwnerContinued:
		for _, m := range []string{"todo", "done", "mixed", "conflict", "stale", "qid"} {
			out = append(out, tev{ev: resumed(0), probes: m})
		}
		out = append(out, tev{ev: resumed(0), badVer: true})
	}
	return out
}

func key(op operation.Op) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|i%d|c%s|", op.Status, op.Kind, op.Index, op.Cause)
	for _, r := range op.Results {
		fmt.Fprintf(&b, "%s%v,", r.State, r.QueueID > 0)
	}
	return b.String()
}

func obligations(before operation.Op, t tev) error {
	next, err := apply(before, t)
	if err == nil {
		return acceptedObligations(before, next, t)
	}
	return nil
}

func acceptedObligations(before, next operation.Op, t tev) error {
	if before.Status == operation.None {
		if next.Version != 0 || !next.UpdatedAt.Equal(t.ev.At) {
			return fmt.Errorf("created at version %d, updated %v", next.Version, next.UpdatedAt)
		}
		return nil
	}
	if next.Version != before.Version+1 {
		return fmt.Errorf("version %d -> %d", before.Version, next.Version)
	}
	if !next.UpdatedAt.Equal(t.ev.At) {
		return fmt.Errorf("updated at %v, want %v", next.UpdatedAt, t.ev.At)
	}
	if len(next.Steps) != len(before.Steps) || len(next.Results) != len(before.Results) {
		return errors.New("shape changed")
	}
	return nil
}

func opMachine() machinetest.Machine[operation.Status, operation.EventKind, operation.Op, tev] {
	return machinetest.Machine[operation.Status, operation.EventKind, operation.Op, tev]{
		States:  append([]operation.Status{operation.None}, operation.Statuses()...),
		Kinds:   operation.EventKinds(),
		Initial: operation.Op{},
		State:   func(o operation.Op) operation.Status { return o.Status },
		Key:     key,
		Samples: samples,
		Events:  eventsFor,
		Matches: func(o operation.Op, t tev) int { return operation.Matches(o.Status, t.ev.Kind) },
		Next:    apply,
		Check:   operation.Check,
		Terminal: func(s operation.Status) bool {
			return s == operation.Done || s == operation.FailedAtStep
		},
		Extra: obligations,
	}
}
