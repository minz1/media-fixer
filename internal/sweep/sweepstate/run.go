package sweepstate

import (
	"errors"
	"fmt"
	"slices"
)

type RunStatus string

const (
	Running     RunStatus = "running"
	Done        RunStatus = "done"
	Paused      RunStatus = "paused"
	Refused     RunStatus = "refused"
	Failed      RunStatus = "failed"
	Interrupted RunStatus = "interrupted"
	Crashed     RunStatus = "crashed"
)

type RunMeta struct {
	Stored      bool
	Settled     bool
	OK          bool
	AnchorsSlot bool
	Retryable   bool
	Tone        string
	Label       string
	Info        string
}

const toneYellow = "yellow"

const crashedInfo = "media-fixer stopped during this run; its counts weren't saved"

func RunStatuses() []RunStatus {
	return []RunStatus{Running, Done, Paused, Refused, Failed, Interrupted, Crashed}
}

func Meta(s RunStatus) RunMeta {
	label := string(s)
	switch s {
	case Running:
		return RunMeta{Stored: true, AnchorsSlot: true, Tone: "blue", Label: label}
	case Done:
		return RunMeta{Stored: true, Settled: true, OK: true, AnchorsSlot: true, Tone: "green", Label: label}
	case Paused:
		return RunMeta{Stored: true, Settled: true, Tone: toneYellow, Label: label}
	case Refused:
		return RunMeta{Stored: true, Settled: true, AnchorsSlot: true, Tone: "red", Label: label}
	case Failed:
		return RunMeta{Stored: true, Settled: true, Retryable: true, Tone: "red", Label: label}
	case Interrupted:
		return RunMeta{Stored: true, Settled: true, Retryable: true, Tone: toneYellow, Label: label}
	case Crashed:
		return RunMeta{Settled: true, Retryable: true, Tone: toneYellow, Label: string(Interrupted), Info: crashedInfo}
	}
	return RunMeta{}
}

func StatusesWhere(pred func(RunMeta) bool) []RunStatus {
	var out []RunStatus
	for _, s := range RunStatuses() {
		if pred(Meta(s)) {
			out = append(out, s)
		}
	}
	return out
}

func Derive(stored RunStatus, hasCounts bool) RunStatus {
	if stored == Interrupted && !hasCounts {
		return Crashed
	}
	return stored
}

type Outcome int

const (
	OutcomeOK Outcome = iota
	OutcomePaused
	OutcomeRedownload
	OutcomeCancelled
	OutcomeError
)

type RunEventKind int

const (
	EvFinished RunEventKind = iota
	EvReconciled
)

type RunEvent struct {
	Kind    RunEventKind
	Outcome Outcome
	Reason  string
}

type RunSnap struct {
	Status      RunStatus
	HasCounts   bool
	HasFinished bool
	Reason      string
}

type RunRow struct {
	From    []RunStatus
	Kind    RunEventKind
	Outcome *Outcome
	To      RunStatus
	Reject  string
}

func settledStatuses() []RunStatus {
	return StatusesWhere(func(m RunMeta) bool { return m.Settled })
}

func runTransitions() []RunRow {
	from := []RunStatus{Running}
	return []RunRow{
		{From: from, Kind: EvFinished, Outcome: new(OutcomeOK), To: Done},
		{From: from, Kind: EvFinished, Outcome: new(OutcomePaused), To: Paused},
		{From: from, Kind: EvFinished, Outcome: new(OutcomeRedownload), To: Refused},
		{From: from, Kind: EvFinished, Outcome: new(OutcomeCancelled), To: Interrupted},
		{From: from, Kind: EvFinished, Outcome: new(OutcomeError), To: Failed},
		{From: from, Kind: EvReconciled, To: Crashed},
		{From: settledStatuses(), Kind: EvFinished, Reject: "sweep run already settled"},
		{From: settledStatuses(), Kind: EvReconciled, Reject: "sweep run already settled"},
	}
}

func runRowMatches(r RunRow, cur RunSnap, ev RunEvent) bool {
	if r.Kind != ev.Kind || !slices.Contains(r.From, cur.Status) {
		return false
	}
	return r.Outcome == nil || ev.Kind != EvFinished || *r.Outcome == ev.Outcome
}

func RunRowsFor(cur RunSnap, ev RunEvent) int {
	n := 0
	for _, r := range runTransitions() {
		if runRowMatches(r, cur, ev) {
			n++
		}
	}
	return n
}

func needsReason(s RunStatus) bool {
	return s == Refused || s == Failed || s == Interrupted
}

func NextRun(cur RunSnap, ev RunEvent) (RunSnap, error) {
	var hit *RunRow
	rows := runTransitions()
	for i := range rows {
		if runRowMatches(rows[i], cur, ev) {
			if hit != nil {
				return cur, fmt.Errorf("sweep run %s: more than one transition matches %+v", cur.Status, ev)
			}
			hit = &rows[i]
		}
	}
	if hit == nil {
		return cur, fmt.Errorf("sweep run %s: no transition for %+v", cur.Status, ev)
	}
	if hit.Reject != "" {
		return cur, errors.New(hit.Reject)
	}
	next := RunSnap{Status: hit.To}
	if ev.Kind == EvFinished {
		next.HasCounts, next.HasFinished = true, true
		if needsReason(hit.To) {
			next.Reason = ev.Reason
		}
	}
	if err := CheckRun(next); err != nil {
		return cur, err
	}
	return next, nil
}

func CheckRun(s RunSnap) error {
	switch s.Status {
	case Running:
		if s.HasCounts || s.HasFinished || s.Reason != "" {
			return errors.New("running sweep run has counts, a finish time or a reason")
		}
	case Done, Paused:
		if !s.HasCounts || !s.HasFinished || s.Reason != "" {
			return fmt.Errorf("%s sweep run needs counts and a finish time and no reason", s.Status)
		}
	case Refused, Failed, Interrupted:
		if !s.HasCounts || !s.HasFinished || s.Reason == "" {
			return fmt.Errorf("%s sweep run needs counts, a finish time and a reason", s.Status)
		}
	case Crashed:
		if s.HasCounts || s.HasFinished {
			return errors.New("crashed sweep run has counts or a finish time")
		}
	default:
		return fmt.Errorf("unknown sweep run status %q", s.Status)
	}
	return nil
}
