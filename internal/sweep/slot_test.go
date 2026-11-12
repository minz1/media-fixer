package sweep_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/sweep"
)

func slotAtTime() time.Time { return time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC) }

var (
	errCause   = errors.New("cause")
	errUnknown = errors.New("unknown row")
)

func slotStates() []sweep.SlotState {
	return []sweep.SlotState{sweep.SlotIdle, sweep.SlotLive, sweep.SlotDry, sweep.SlotStopped}
}

func slotKinds() []sweep.SlotKind {
	return []sweep.SlotKind{sweep.SlotStart, sweep.SlotDone, sweep.SlotCancelLive, sweep.SlotReconcile, sweep.SlotStop}
}

func slotSamples(s sweep.SlotState) []sweep.SlotSnap {
	switch s {
	case sweep.SlotLive:
		return []sweep.SlotSnap{
			{State: s, Trigger: "manual", Since: slotAtTime()},
			{State: s, Trigger: "schedule", Since: slotAtTime().Add(time.Hour)},
		}
	case sweep.SlotDry:
		return []sweep.SlotSnap{{State: s, Trigger: "manual", Dry: true, Since: slotAtTime()}}
	case sweep.SlotIdle, sweep.SlotStopped:
		return []sweep.SlotSnap{{State: s}}
	}
	return nil
}

func slotEvents(k sweep.SlotKind) []sweep.SlotEvent {
	var out []sweep.SlotEvent
	for _, dry := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, paused := range []bool{false, true} {
				out = append(out, sweep.SlotEvent{
					Kind: k, Trigger: "manual", Dry: dry, Enabled: enabled, Paused: paused,
					At: slotAtTime(), Cause: errCause,
				})
			}
		}
	}
	return out
}

func slotNext(s sweep.SlotSnap, ev sweep.SlotEvent) (sweep.SlotSnap, error) {
	next, _, err := sweep.NextSlot(s, ev)
	return next, err
}

func slotMatches(s sweep.SlotSnap, ev sweep.SlotEvent) int {
	next, effs, err := sweep.NextSlot(s, ev)
	if err != nil && (next != s || len(effs) != 0) {
		return 0
	}
	if err != nil && !knownRejection(err) {
		return 0
	}
	return 1
}

func knownRejection(err error) bool {
	for _, want := range []error{
		sweep.ErrDisabled, sweep.ErrPaused, sweep.ErrRunActive, card.ErrRestarting,
		sweep.ErrSlotNoRun, sweep.ErrSlotBusy, sweep.ErrSlotStopped,
	} {
		if errors.Is(err, want) {
			return true
		}
	}
	return false
}

type slotWant struct {
	snap sweep.SlotSnap
	effs []sweep.EffKind
	err  error
}

func slotOracle(s sweep.SlotSnap, ev sweep.SlotEvent) slotWant {
	run := sweep.SlotLive
	if ev.Dry {
		run = sweep.SlotDry
	}
	started := sweep.SlotSnap{State: run, Trigger: ev.Trigger, Dry: ev.Dry, Since: ev.At}
	idle := sweep.SlotSnap{State: sweep.SlotIdle}
	stopped := sweep.SlotSnap{State: sweep.SlotStopped}
	switch s.State {
	case sweep.SlotIdle:
		switch ev.Kind {
		case sweep.SlotStart:
			switch {
			case !ev.Dry && !ev.Enabled:
				return slotWant{snap: s, err: sweep.ErrDisabled}
			case ev.Paused:
				return slotWant{snap: s, err: sweep.ErrPaused}
			}
			return slotWant{snap: started, effs: []sweep.EffKind{sweep.EffLaunch}}
		case sweep.SlotDone:
			return slotWant{snap: s, err: sweep.ErrSlotNoRun}
		case sweep.SlotCancelLive:
			return slotWant{snap: idle}
		case sweep.SlotReconcile:
			return slotWant{snap: idle, effs: []sweep.EffKind{sweep.EffCrashStale}}
		case sweep.SlotStop:
			return slotWant{snap: stopped}
		}
	case sweep.SlotLive, sweep.SlotDry:
		switch ev.Kind {
		case sweep.SlotStart:
			if !ev.Dry && !ev.Enabled {
				return slotWant{snap: s, err: sweep.ErrDisabled}
			}
			return slotWant{snap: s, err: sweep.ErrRunActive}
		case sweep.SlotDone:
			return slotWant{snap: idle}
		case sweep.SlotCancelLive:
			if s.State == sweep.SlotLive {
				return slotWant{snap: s, effs: []sweep.EffKind{sweep.EffCancel}}
			}
			return slotWant{snap: s}
		case sweep.SlotReconcile:
			return slotWant{snap: s, err: sweep.ErrSlotBusy}
		case sweep.SlotStop:
			return slotWant{snap: stopped}
		}
	case sweep.SlotStopped:
		return stoppedWant(s, ev)
	}
	return slotWant{snap: s, err: errUnknown}
}

func stoppedWant(s sweep.SlotSnap, ev sweep.SlotEvent) slotWant {
	switch ev.Kind {
	case sweep.SlotDone:
		return slotWant{snap: s}
	case sweep.SlotStop:
		return slotWant{snap: s, err: sweep.ErrSlotStopped}
	case sweep.SlotStart, sweep.SlotCancelLive, sweep.SlotReconcile:
		return slotWant{snap: s, err: card.ErrRestarting}
	}
	return slotWant{snap: s, err: errUnknown}
}

func slotOwes(s sweep.SlotSnap, ev sweep.SlotEvent) error {
	got, effs, err := sweep.NextSlot(s, ev)
	want := slotOracle(s, ev)
	switch {
	case !errors.Is(err, want.err) || (err == nil) != (want.err == nil):
		return fmt.Errorf("error %s, want %s", fmt.Sprint(err), fmt.Sprint(want.err))
	case got != want.snap:
		return fmt.Errorf("snapshot %+v, want %+v", got, want.snap)
	case len(effs) != len(want.effs):
		return fmt.Errorf("effects %+v, want %v", effs, want.effs)
	}
	for i, e := range effs {
		if e.Kind != want.effs[i] {
			return fmt.Errorf("effect %d is %v, want %v", i, e.Kind, want.effs[i])
		}
		if e.Kind == sweep.EffCancel && !errors.Is(e.Cause, errCause) {
			return fmt.Errorf("cancel effect lost its cause: %w", e.Cause)
		}
	}
	return nil
}

func slotMachine() machinetest.Machine[sweep.SlotState, sweep.SlotKind, sweep.SlotSnap, sweep.SlotEvent] {
	return machinetest.Machine[sweep.SlotState, sweep.SlotKind, sweep.SlotSnap, sweep.SlotEvent]{
		States:   slotStates(),
		Kinds:    slotKinds(),
		Initial:  sweep.SlotSnap{State: sweep.SlotIdle},
		State:    func(s sweep.SlotSnap) sweep.SlotState { return s.State },
		Key:      func(s sweep.SlotSnap) string { return string(s.State) },
		Samples:  slotSamples,
		Events:   slotEvents,
		Matches:  slotMatches,
		Next:     slotNext,
		Check:    sweep.CheckSlot,
		Terminal: func(s sweep.SlotState) bool { return s == sweep.SlotStopped },
		Extra:    slotOwes,
	}
}

func TestSlotMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, slotMachine())
	for _, s := range slotStates() {
		for _, snap := range slotSamples(s) {
			for _, k := range slotKinds() {
				for _, ev := range slotEvents(k) {
					if err := slotOwes(snap, ev); err != nil {
						t.Errorf("state %s, event %+v: %v", s, ev, err)
					}
				}
			}
		}
	}
}

func TestSlotMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, slotMachine())
}

func TestSlotMachine_Invariants(t *testing.T) {
	t.Parallel()
	machinetest.Invariants(t, slotMachine())
}

func FuzzSlotMachine(f *testing.F) {
	m := slotMachine()
	for i := range 8 {
		f.Add([]byte{byte(i)})
	}
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6})
	f.Add([]byte{4, 40, 12, 20, 36, 28})
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func start(trigger string, dry, enabled, paused bool) sweep.SlotEvent {
	return sweep.SlotEvent{
		Kind: sweep.SlotStart, Trigger: trigger, Dry: dry, Enabled: enabled, Paused: paused, At: slotAtTime(),
	}
}

func TestS1_1_StartRefusedWhileActive(t *testing.T) {
	t.Parallel()
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry=%v", dry), func(t *testing.T) {
			t.Parallel()
			idle := sweep.SlotSnap{State: sweep.SlotIdle}
			running, effs, err := sweep.NextSlot(idle, start("manual", dry, true, false))
			if err != nil || len(effs) != 1 || effs[0].Kind != sweep.EffLaunch {
				t.Fatalf("first start: effects %+v, err %v", effs, err)
			}
			for _, second := range []bool{false, true} {
				next, refused, serr := sweep.NextSlot(running, start("schedule", second, true, false))
				if !errors.Is(serr, sweep.ErrRunActive) {
					t.Errorf("second start (dry=%v) while active gave %v, want ErrRunActive", second, serr)
				}
				if next != running || len(refused) != 0 {
					t.Errorf("refused start changed the slot to %+v or launched %+v", next, refused)
				}
			}
		})
	}
}

func TestS1_16_CancelLiveSparesDryRunRow(t *testing.T) {
	t.Parallel()
	idle := sweep.SlotSnap{State: sweep.SlotIdle}
	cancel := sweep.SlotEvent{Kind: sweep.SlotCancelLive, At: slotAtTime(), Cause: sweep.ErrDisabled}

	dry, _, _ := sweep.NextSlot(idle, start("manual", true, true, false))
	next, effs, err := sweep.NextSlot(dry, cancel)
	if err != nil || next != dry || len(effs) != 0 {
		t.Errorf("cancel on a dry run: slot %+v, effects %+v, err %v; want no change and no effect", next, effs, err)
	}

	live, _, _ := sweep.NextSlot(idle, start("manual", false, true, false))
	next, effs, err = sweep.NextSlot(live, cancel)
	if err != nil || next != live || len(effs) != 1 || effs[0].Kind != sweep.EffCancel ||
		!errors.Is(effs[0].Cause, sweep.ErrDisabled) {
		t.Errorf("cancel on a live run: slot %+v, effects %+v, err %v; want one cancel with the cause", next, effs, err)
	}
}

func TestS1_11_ReconcileOnlyFromIdle(t *testing.T) {
	t.Parallel()
	reconcile := sweep.SlotEvent{Kind: sweep.SlotReconcile, At: slotAtTime()}
	for _, s := range slotStates() {
		for _, snap := range slotSamples(s) {
			next, effs, err := sweep.NextSlot(snap, reconcile)
			crashed := len(effs) == 1 && effs[0].Kind == sweep.EffCrashStale
			if s == sweep.SlotIdle {
				if err != nil || !crashed || next != snap {
					t.Errorf("idle reconcile: slot %+v, effects %+v, err %v", next, effs, err)
				}
				continue
			}
			if err == nil || len(effs) != 0 || next != snap {
				t.Errorf("%s reconcile: slot %+v, effects %+v, err %v; want a refusal", s, next, effs, err)
			}
		}
	}
}

func TestS2_6_RefusalOrderMatchesToday(t *testing.T) {
	t.Parallel()
	idle := sweep.SlotSnap{State: sweep.SlotIdle}
	active, _, _ := sweep.NextSlot(idle, start("manual", false, true, false))
	cases := []struct {
		name    string
		dry     bool
		enabled bool
		active  bool
		paused  bool
		want    error
	}{
		{"live disabled", false, false, false, false, sweep.ErrDisabled},
		{"live disabled and active", false, false, true, false, sweep.ErrDisabled},
		{"live disabled and paused", false, false, false, true, sweep.ErrDisabled},
		{"live disabled active paused", false, false, true, true, sweep.ErrDisabled},
		{"live active", false, true, true, false, sweep.ErrRunActive},
		{"live active and paused", false, true, true, true, sweep.ErrRunActive},
		{"live paused", false, true, false, true, sweep.ErrPaused},
		{"live free", false, true, false, false, nil},
		{"dry disabled free", true, false, false, false, nil},
		{"dry disabled active", true, false, true, false, sweep.ErrRunActive},
		{"dry disabled paused", true, false, false, true, sweep.ErrPaused},
		{"dry disabled active paused", true, false, true, true, sweep.ErrRunActive},
		{"dry active", true, true, true, false, sweep.ErrRunActive},
		{"dry paused", true, true, false, true, sweep.ErrPaused},
		{"dry free", true, true, false, false, nil},
	}
	for _, c := range cases {
		for _, trigger := range []string{"manual", "schedule"} {
			from := idle
			if c.active {
				from = active
			}
			_, _, err := sweep.NextSlot(from, start(trigger, c.dry, c.enabled, c.paused))
			if !errors.Is(err, c.want) || (err == nil) != (c.want == nil) {
				t.Errorf("%s via %s: %v, want %v", c.name, trigger, err, c.want)
			}
		}
	}
}

func TestSlot_StoppedRefusesWithRestarting(t *testing.T) {
	t.Parallel()
	stopped := sweep.SlotSnap{State: sweep.SlotStopped}
	for _, dry := range []bool{false, true} {
		for _, k := range []sweep.SlotKind{sweep.SlotStart, sweep.SlotCancelLive, sweep.SlotReconcile} {
			ev := start("manual", dry, true, false)
			ev.Kind = k
			next, effs, err := sweep.NextSlot(stopped, ev)
			if !errors.Is(err, card.ErrRestarting) || next != stopped || len(effs) != 0 {
				t.Errorf("%s (dry=%v) on a stopped slot: slot %+v, effects %+v, err %v", k, dry, next, effs, err)
			}
		}
	}
	_, _, err := sweep.NextSlot(stopped, sweep.SlotEvent{Kind: sweep.SlotStop})
	if !errors.Is(err, sweep.ErrSlotStopped) {
		t.Errorf("second stop gave %v, want already_stopped", err)
	}
}

func TestSlotMachine_Idempotence(t *testing.T) {
	t.Parallel()
	machinetest.Idempotence(t, slotMachine(), []sweep.SlotKind{sweep.SlotCancelLive, sweep.SlotReconcile})
}

func TestMachineDiagram_Slot(t *testing.T) {
	t.Parallel()
	machinetest.GoldenMermaid(t, slotMachine(), "testdata/slot.md")
}
