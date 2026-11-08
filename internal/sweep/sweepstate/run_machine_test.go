package sweepstate_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func runSamples(s sweepstate.RunStatus) []sweepstate.RunSnap {
	switch s {
	case sweepstate.Running, sweepstate.Crashed:
		return []sweepstate.RunSnap{{Status: s}}
	case sweepstate.Done, sweepstate.Paused:
		return []sweepstate.RunSnap{{Status: s, HasCounts: true, HasFinished: true}}
	case sweepstate.Refused, sweepstate.Failed, sweepstate.Interrupted:
		return []sweepstate.RunSnap{
			{Status: s, HasCounts: true, HasFinished: true, Reason: "why"},
			{Status: s, HasCounts: true, HasFinished: true, Reason: "other"},
		}
	}
	return nil
}

func runOutcomes() []sweepstate.Outcome {
	return []sweepstate.Outcome{
		sweepstate.OutcomeOK, sweepstate.OutcomePaused, sweepstate.OutcomeRedownload,
		sweepstate.OutcomeCancelled, sweepstate.OutcomeError,
	}
}

func runEvents(k sweepstate.RunEventKind) []sweepstate.RunEvent {
	if k == sweepstate.EvReconciled {
		return []sweepstate.RunEvent{{Kind: k}}
	}
	var out []sweepstate.RunEvent
	for _, o := range runOutcomes() {
		for _, reason := range []string{"", "why"} {
			out = append(out, sweepstate.RunEvent{Kind: k, Outcome: o, Reason: reason})
		}
	}
	return out
}

func runMachine() machinetest.Machine[sweepstate.RunStatus, sweepstate.RunEventKind, sweepstate.RunSnap, sweepstate.RunEvent] {
	return machinetest.Machine[sweepstate.RunStatus, sweepstate.RunEventKind, sweepstate.RunSnap, sweepstate.RunEvent]{
		States:  sweepstate.RunStatuses(),
		Kinds:   []sweepstate.RunEventKind{sweepstate.EvFinished, sweepstate.EvReconciled},
		Initial: sweepstate.RunSnap{Status: sweepstate.Running},
		State:   func(s sweepstate.RunSnap) sweepstate.RunStatus { return s.Status },
		Key:     func(s sweepstate.RunSnap) string { return fmt.Sprintf("%+v", s) },
		Samples: runSamples,
		Events:  runEvents,
		Matches: sweepstate.RunRowsFor,
		Next:    sweepstate.NextRun,
		Check:   sweepstate.CheckRun,
		Terminal: func(s sweepstate.RunStatus) bool {
			return sweepstate.Meta(s).Settled
		},
	}
}

func TestRunMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, runMachine())
}

func TestRunMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, runMachine())
	start := sweepstate.RunSnap{Status: sweepstate.Running}
	want := make(map[sweepstate.RunStatus]bool)
	for _, k := range runMachine().Kinds {
		for _, ev := range runEvents(k) {
			if next, err := sweepstate.NextRun(start, ev); err == nil {
				want[next.Status] = true
			}
		}
	}
	for _, s := range sweepstate.RunStatuses() {
		if s != sweepstate.Running && !want[s] {
			t.Errorf("%s is not reachable from running in one event", s)
		}
	}
	if len(want) != 6 {
		t.Errorf("running has %d distinct exits, want 6 (R1-R6)", len(want))
	}
}

func TestRunMachine_Invariants(t *testing.T) {
	t.Parallel()
	machinetest.Invariants(t, runMachine())
}

func TestRunMachine_FinishNeedsReason(t *testing.T) {
	t.Parallel()
	start := sweepstate.RunSnap{Status: sweepstate.Running}
	for _, o := range []sweepstate.Outcome{sweepstate.OutcomeRedownload, sweepstate.OutcomeCancelled, sweepstate.OutcomeError} {
		next, err := sweepstate.NextRun(start, sweepstate.RunEvent{Kind: sweepstate.EvFinished, Outcome: o})
		if err == nil || next != start {
			t.Errorf("outcome %d with no reason: got %+v, %v", o, next, err)
		}
	}
	next, err := sweepstate.NextRun(start, sweepstate.RunEvent{Kind: sweepstate.EvFinished, Reason: "late"})
	if err != nil || next.Reason != "" || next.Status != sweepstate.Done {
		t.Errorf("OK finish: got %+v, %v", next, err)
	}
	next, err = sweepstate.NextRun(start, sweepstate.RunEvent{Kind: sweepstate.EvReconciled})
	if err != nil || next.Status != sweepstate.Crashed || next.HasCounts || next.HasFinished || next.Reason != "" {
		t.Errorf("reconcile: got %+v, %v", next, err)
	}
}

func TestRunMachine_SettledRejects(t *testing.T) {
	t.Parallel()
	for _, s := range sweepstate.StatusesWhere(func(m sweepstate.RunMeta) bool { return m.Settled }) {
		for _, snap := range runSamples(s) {
			for _, k := range runMachine().Kinds {
				for _, ev := range runEvents(k) {
					if next, err := sweepstate.NextRun(snap, ev); err == nil || next != snap {
						t.Errorf("%s + %+v: got %+v, %v, want a rejection", s, ev, next, err)
					}
				}
			}
		}
	}
}

func TestCheckRun_Table(t *testing.T) {
	t.Parallel()
	full := func(s sweepstate.RunStatus, reason string) sweepstate.RunSnap {
		return sweepstate.RunSnap{Status: s, HasCounts: true, HasFinished: true, Reason: reason}
	}
	cases := []struct {
		name string
		snap sweepstate.RunSnap
		ok   bool
	}{
		{"running", sweepstate.RunSnap{Status: sweepstate.Running}, true},
		{"running with counts", sweepstate.RunSnap{Status: sweepstate.Running, HasCounts: true}, false},
		{"running with reason", sweepstate.RunSnap{Status: sweepstate.Running, Reason: "x"}, false},
		{"done", full(sweepstate.Done, ""), true},
		{"done with reason", full(sweepstate.Done, "x"), false},
		{"paused without finish", sweepstate.RunSnap{Status: sweepstate.Paused, HasCounts: true}, false},
		{"failed", full(sweepstate.Failed, "x"), true},
		{"failed without reason", full(sweepstate.Failed, ""), false},
		{"refused without reason", full(sweepstate.Refused, ""), false},
		{"interrupted", full(sweepstate.Interrupted, "x"), true},
		{
			"interrupted without counts",
			sweepstate.RunSnap{Status: sweepstate.Interrupted, HasFinished: true, Reason: "x"},
			false,
		},
		{"crashed", sweepstate.RunSnap{Status: sweepstate.Crashed}, true},
		{"crashed with counts", sweepstate.RunSnap{Status: sweepstate.Crashed, HasCounts: true}, false},
		{"crashed with finish", sweepstate.RunSnap{Status: sweepstate.Crashed, HasFinished: true}, false},
		{"unknown", sweepstate.RunSnap{Status: "bogus"}, false},
	}
	for _, c := range cases {
		if err := sweepstate.CheckRun(c.snap); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
}

func TestDerive(t *testing.T) {
	t.Parallel()
	for _, s := range sweepstate.RunStatuses() {
		for _, counts := range []bool{false, true} {
			want := s
			if s == sweepstate.Interrupted && !counts {
				want = sweepstate.Crashed
			}
			if got := sweepstate.Derive(s, counts); got != want {
				t.Errorf("Derive(%s, %v) = %s, want %s", s, counts, got, want)
			}
		}
	}
}

func FuzzRunMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	f.Add([]byte{1, 0, 2, 4, 6})
	f.Add([]byte{5, 2, 8, 9, 0})
	f.Add([]byte{3, 4, 255, 0, 10, 10})
	m := runMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func sortedStatuses(in []sweepstate.RunStatus) []sweepstate.RunStatus {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func TestS1_Copies_MetaCoversEveryStatus(t *testing.T) {
	t.Parallel()
	for _, s := range sweepstate.RunStatuses() {
		m := sweepstate.Meta(s)
		if m.Tone == "" || m.Label == "" {
			t.Errorf("%s: tone %q, label %q", s, m.Tone, m.Label)
		}
		if s != sweepstate.Crashed && (m.Label != string(s) || m.Info != "") {
			t.Errorf("%s: label %q, info %q", s, m.Label, m.Info)
		}
	}
	stored := sortedStatuses(sweepstate.StatusesWhere(func(m sweepstate.RunMeta) bool { return m.Stored }))
	want := sortedStatuses([]sweepstate.RunStatus{
		"running", "done", "paused", "refused", "failed", "interrupted",
	})
	if !slices.Equal(stored, want) {
		t.Errorf("stored = %v, want %v", stored, want)
	}
	c := sweepstate.Meta(sweepstate.Crashed)
	if c.Stored || c.Label != "interrupted" || c.Info == "" {
		t.Errorf("crashed meta = %+v", c)
	}
}

func TestS1_Copies_SlotSetsUnchanged(t *testing.T) {
	t.Parallel()
	free := sortedStatuses(sweepstate.StatusesWhere(func(m sweepstate.RunMeta) bool {
		return !m.AnchorsSlot && m.Stored
	}))
	if want := sortedStatuses([]sweepstate.RunStatus{"paused", "interrupted", "failed"}); !slices.Equal(free, want) {
		t.Errorf("not anchoring = %v, want %v", free, want)
	}
	retry := sortedStatuses(sweepstate.StatusesWhere(func(m sweepstate.RunMeta) bool {
		return m.Retryable && m.Stored
	}))
	if want := sortedStatuses([]sweepstate.RunStatus{"failed", "interrupted"}); !slices.Equal(retry, want) {
		t.Errorf("retryable = %v, want %v", retry, want)
	}
}
