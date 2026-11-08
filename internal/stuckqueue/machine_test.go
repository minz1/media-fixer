package stuckqueue_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const smMinAge = time.Hour

func smT0() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

func smAt(d time.Duration) time.Time { return smT0().Add(d) }

func smTimes() []time.Time {
	return []time.Time{smAt(0), smAt(11 * time.Minute), smAt(2 * time.Hour), smAt(17 * 24 * time.Hour)}
}

func smKey() stuckqueue.Key {
	return stuckqueue.Key{App: stuckqueue.AppSonarr, DownloadID: "dl1"}
}

func smBase(st stuckqueue.State) stuckqueue.Snapshot {
	return stuckqueue.Snapshot{Key: smKey(), State: st, FirstSeen: smT0()}
}

func smUntracked() stuckqueue.Snapshot {
	return stuckqueue.Snapshot{Key: smKey(), State: stuckqueue.StateUntracked}
}

func smWaiting() stuckqueue.Snapshot {
	s := smBase(stuckqueue.StateWaiting)
	s.Offered = stuckqueue.OfferRemove
	return s
}

func smDue() stuckqueue.Snapshot {
	s := smBase(stuckqueue.StateDue)
	s.Offered, s.TargetsFP = stuckqueue.OfferAll, "fp"
	return s
}

func smHeld() stuckqueue.Snapshot {
	s := smBase(stuckqueue.StateHeld)
	s.Offered = stuckqueue.OfferRemove
	return s
}

func smCleared() stuckqueue.Snapshot {
	s := smBase(stuckqueue.StateCleared)
	s.Offered = stuckqueue.OfferRemove
	return s
}

func smNotified() stuckqueue.Snapshot {
	s := smBase(stuckqueue.StateNotified)
	s.NotifiedAt, s.Offered, s.TargetsFP = smT0(), stuckqueue.OfferAll, "fp"
	return s
}

func smNoted(res stuckqueue.ActResult, f stuckqueue.Files) stuckqueue.Snapshot {
	s := smNotified()
	s.Acted = &stuckqueue.Acted{Action: stuckqueue.ActionImport, At: smT0(), Result: res, Files: f}
	return s
}

func smInflight() stuckqueue.Snapshot {
	s := smBase(stuckqueue.StateImportInflight)
	s.Import = &stuckqueue.ImportCmd{CommandID: 7, StartedAt: smT0(), Files: stuckqueue.Files{N: 2, M: 2}}
	return s
}

func smActed(a stuckqueue.Action, res stuckqueue.ActResult, f stuckqueue.Files, op int64) stuckqueue.Snapshot {
	s := smBase(stuckqueue.StateActed)
	s.Acted = &stuckqueue.Acted{Action: a, At: smT0(), Result: res, Files: f, OpID: op}
	return s
}

func smActedRemove() stuckqueue.Snapshot {
	return smActed(stuckqueue.ActionRemove, stuckqueue.ResultOK, stuckqueue.Files{}, 0)
}

func smActedImport(n, m int) stuckqueue.Snapshot {
	return smActed(stuckqueue.ActionImport, stuckqueue.ResultOK, stuckqueue.Files{N: n, M: m}, 0)
}

func smActedRegrab(res stuckqueue.ActResult, op int64) stuckqueue.Snapshot {
	return smActed(stuckqueue.ActionRegrab, res, stuckqueue.Files{}, op)
}

func smAll() []stuckqueue.Snapshot {
	miss := func(s stuckqueue.Snapshot) stuckqueue.Snapshot { s.Misses = 1; return s }
	late := smNotified()
	late.NotifiedAt = smAt(2 * time.Hour)
	warm := smWaiting()
	warm.NotifiedAt = smT0()
	started := smInflight()
	started.Import.StartedAt = smAt(2 * time.Hour)
	return []stuckqueue.Snapshot{
		smUntracked(),
		smWaiting(), miss(smWaiting()), warm,
		smDue(), miss(smDue()),
		smHeld(), miss(smHeld()),
		smNotified(), miss(smNotified()), late,
		smNoted(stuckqueue.ResultImportFailed, stuckqueue.Files{N: 2, M: 2}),
		smNoted(stuckqueue.ResultPartial, stuckqueue.Files{N: 1, M: 2}),
		smInflight(), started,
		smActedRemove(), smActedImport(2, 2), smActedImport(1, 2),
		smActedRegrab(stuckqueue.ResultOK, 1), smActedRegrab(stuckqueue.ResultUnfinished, 1),
		smCleared(), miss(smCleared()),
	}
}

func smSamples(st stuckqueue.State) []stuckqueue.Snapshot {
	var out []stuckqueue.Snapshot
	for _, s := range smAll() {
		if s.State == st {
			out = append(out, s)
		}
	}
	return out
}

func smKind(k stuckqueue.EventKind, at time.Time) stuckqueue.Event {
	return stuckqueue.Event{Kind: k, At: at, MinAge: smMinAge}
}

func smSeen(at time.Time, v stuckqueue.Verdict, hold bool, off stuckqueue.Offer) stuckqueue.Event {
	e := smKind(stuckqueue.EvSeen, at)
	e.Hold, e.Obs = hold, stuckqueue.Observation{FP: "fp", Verdict: v, Offered: off}
	return e
}

func smActedEv(at time.Time, a stuckqueue.Action, op int64, resumable bool, f stuckqueue.Files) stuckqueue.Event {
	e := smKind(stuckqueue.EvActed, at)
	e.Action, e.OpID, e.Resumable, e.Files = a, op, resumable, f
	return e
}

func smPosted(at time.Time, id int, mapped bool, f stuckqueue.Files) stuckqueue.Event {
	e := smKind(stuckqueue.EvImportPosted, at)
	e.CommandID, e.Mapped, e.Files = id, mapped, f
	return e
}

func smObserved(at time.Time, c stuckqueue.CmdStatus) stuckqueue.Event {
	e := smKind(stuckqueue.EvCommandObserved, at)
	e.Cmd, e.Timeout = c, time.Hour
	return e
}

func smOpEv(k stuckqueue.EventKind, at time.Time, op int64, done bool) stuckqueue.Event {
	e := smKind(k, at)
	e.OpID, e.Done = op, done
	return e
}

func smFailed(at, claim time.Time) stuckqueue.Event {
	e := smKind(stuckqueue.EvNotifyFailed, at)
	e.ClaimAt = claim
	return e
}

func smKeyOf(s stuckqueue.Snapshot) string {
	k := fmt.Sprintf("%s|%s|%d|%d|%d|%s|%s", s.Key.DownloadID, s.State, s.FirstSeen.Unix(), s.NotifiedAt.Unix(),
		s.Misses, s.TargetsFP, s.Offered)
	if a := s.Acted; a != nil {
		k += fmt.Sprintf("|A%s,%d,%s,%d/%d,%d", a.Action, a.At.Unix(), a.Result, a.Files.N, a.Files.M, a.OpID)
	}
	if i := s.Import; i != nil {
		k += fmt.Sprintf("|I%d,%d,%d/%d", i.CommandID, i.StartedAt.Unix(), i.Files.N, i.Files.M)
	}
	return k
}

func smApply(s stuckqueue.Snapshot, e stuckqueue.Event) (stuckqueue.Snapshot, error) {
	next, _, err := stuckqueue.Next(s, e)
	return next, err
}

func smObligations(before stuckqueue.Snapshot, e stuckqueue.Event) error {
	next, effs, err := stuckqueue.Next(before, e)
	if err == nil {
		return smOwes(before, e, next, effs)
	}
	return nil
}

func smOwes(before stuckqueue.Snapshot, e stuckqueue.Event, next stuckqueue.Snapshot, effs []stuckqueue.Effect) error {
	refreshAt, clearAt := -1, -1
	for i, f := range effs {
		switch f.(type) {
		case stuckqueue.EffRefreshCard:
			refreshAt = i
		case stuckqueue.EffClearDownload:
			clearAt = i
		}
	}
	if clearAt >= 0 && (next.State != stuckqueue.StateHeld || e.Hold || e.Kind != stuckqueue.EvSeen) {
		return fmt.Errorf("clear emitted into %s", next.State)
	}
	if clearAt > refreshAt && refreshAt >= 0 {
		return fmt.Errorf("refresh before clear: %v", effs)
	}
	switch e.Kind {
	case stuckqueue.EvActed, stuckqueue.EvImportPosted, stuckqueue.EvAdopted, stuckqueue.EvResumed:
		if refreshAt < 0 {
			return errors.New("owner event accepted without a refresh")
		}
	case stuckqueue.EvSeen:
		if smSeenOwesRefresh(before, next) && refreshAt < 0 {
			return errors.New("card changed without a refresh")
		}
	case stuckqueue.EvMissed, stuckqueue.EvClaimed, stuckqueue.EvNotifyFailed, stuckqueue.EvClearDone,
		stuckqueue.EvClearFailed, stuckqueue.EvCommandObserved, stuckqueue.EvExpire:
	}
	return nil
}

func smCardChanged(a, b stuckqueue.Snapshot) bool {
	if a.State != b.State || a.Offered != b.Offered || a.TargetsFP != b.TargetsFP {
		return true
	}
	return !reflect.DeepEqual(a.Acted, b.Acted)
}

func smMachine() machinetest.Machine[stuckqueue.State, stuckqueue.EventKind, stuckqueue.Snapshot, stuckqueue.Event] {
	return machinetest.Machine[stuckqueue.State, stuckqueue.EventKind, stuckqueue.Snapshot, stuckqueue.Event]{
		States:   stuckqueue.States(),
		Kinds:    stuckqueue.EventKinds(),
		Initial:  smUntracked(),
		State:    func(s stuckqueue.Snapshot) stuckqueue.State { return s.State },
		Key:      smKeyOf,
		Samples:  smSamples,
		Events:   smEventsFor,
		Matches:  stuckqueue.Matches,
		Next:     smApply,
		Check:    stuckqueue.Check,
		Terminal: func(s stuckqueue.State) bool { return stuckqueue.Meta(stuckqueue.Snapshot{State: s}).Terminal },
		Extra:    smObligations,
	}
}

func TestStuckMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, smMachine())
}

func TestStuckMachine_ReachabilityAndExits(t *testing.T) {
	t.Parallel()
	m := smMachine()
	if got := stuckqueue.Meta(m.Initial).Terminal; !got {
		t.Errorf("initial %s is not terminal", m.Initial.State)
	}
	for _, s := range stuckqueue.States() {
		if want := s == stuckqueue.StateUntracked; m.Terminal(s) != want {
			t.Errorf("state %s terminal = %v, want %v", s, m.Terminal(s), want)
		}
	}
	machinetest.Reachability(t, m)
}

func TestStuckMachine_Invariants(t *testing.T) {
	t.Parallel()
	for _, s := range smAll() {
		if err := stuckqueue.Check(s); err != nil {
			t.Errorf("sample %s is illegal: %v", smKeyOf(s), err)
		}
	}
	machinetest.Invariants(t, smMachine())
}

func smSeeds() [][]stuckqueue.Event {
	t0, tw, tg, tk := smAt(0), smAt(2*time.Hour), smAt(11*time.Minute), smAt(17*24*time.Hour)
	none := stuckqueue.Files{}
	two := stuckqueue.Files{N: 2, M: 2}
	return [][]stuckqueue.Event{
		{
			smSeen(t0, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smKind(stuckqueue.EvClaimed, tw),
			smFailed(tw, t0),
			smFailed(tw, tw),
		},
		{
			smSeen(t0, stuckqueue.VerdictClear, false, stuckqueue.OfferRemove),
			smSeen(tw, stuckqueue.VerdictClear, false, stuckqueue.OfferRemove),
			smKind(
				stuckqueue.EvClearFailed,
				tw,
			),
			smKind(stuckqueue.EvClearDone, tw),
			smKind(stuckqueue.EvMissed, tw),
			smKind(stuckqueue.EvMissed, tw),
		},
		{
			smSeen(t0, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smActedEv(t0, stuckqueue.ActionRegrab, 1, false, none),
			smSeen(
				tw,
				stuckqueue.VerdictGap,
				false,
				stuckqueue.OfferAll,
			),
			smActedEv(tw, stuckqueue.ActionImport, 0, false, two),
			smSeen(
				tg,
				stuckqueue.VerdictGap,
				false,
				stuckqueue.OfferAll,
			),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
		},
		{
			smSeen(t0, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smPosted(tw, 7, false, two),
			smObserved(tw, stuckqueue.CmdRunning),
			smObserved(tw, stuckqueue.CmdCompleted),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smKind(stuckqueue.EvExpire, tk),
		},
		{
			smSeen(t0, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smPosted(
				tw,
				7,
				false,
				two,
			),
			smObserved(tw, stuckqueue.CmdFailed),
			smPosted(tw, 7, true, stuckqueue.Files{N: 1, M: 2}),
		},
		{
			smOpEv(stuckqueue.EvAdopted, t0, 1, false),
			smOpEv(stuckqueue.EvAdopted, tw, 2, false),
			smOpEv(stuckqueue.EvAdopted, tw, 0, false),
			smOpEv(
				stuckqueue.EvResumed,
				tw,
				2,
				false,
			),
			smOpEv(stuckqueue.EvResumed, tw, 1, true),
			smOpEv(stuckqueue.EvResumed, tw, 0, true),
			smOpEv(stuckqueue.EvResumed, tg, 1, true),
		},
		{
			smActedEv(t0, stuckqueue.ActionRemove, 0, false, none),
			smSeen(tg, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smKind(stuckqueue.EvExpire, tk),
		},
		{
			smActedEv(t0, stuckqueue.ActionRegrab, 1, true, none),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			smKind(
				stuckqueue.EvMissed,
				tw,
			),
			smOpEv(stuckqueue.EvResumed, tg, 1, true),
			smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
		},
	}
}

func FuzzStuckMachine(f *testing.F) {
	m := smMachine()
	flat := slices.Concat(func() [][]stuckqueue.Event {
		var out [][]stuckqueue.Event
		for _, k := range m.Kinds {
			out = append(out, m.Events(k))
		}
		return out
	}()...)
	for i := range flat {
		f.Add([]byte{byte(i)})
	}
	for _, seq := range smSeeds() {
		var data []byte
		for _, e := range seq {
			i := slices.IndexFunc(flat, func(x stuckqueue.Event) bool { return reflect.DeepEqual(x, e) })
			if i < 0 {
				f.Fatalf("seed event %+v is not in the event set", e)
			}
			data = append(data, byte(i))
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func TestStates_StoredMatchesDB(t *testing.T) {
	t.Parallel()
	var stored []string
	for _, s := range stuckqueue.States() {
		if stuckqueue.Meta(stuckqueue.Snapshot{State: s}).Stored {
			stored = append(stored, string(s))
		}
	}
	got, want := slices.Sorted(slices.Values(stored)), slices.Sorted(slices.Values(db.StuckStates()))
	if !slices.Equal(got, want) {
		t.Errorf("stored states %v, db.StuckStates() %v", got, want)
	}
}

func smSeenOwesRefresh(before, next stuckqueue.Snapshot) bool {
	switch before.State {
	case stuckqueue.StateUntracked:
		return false
	case stuckqueue.StateImportInflight, stuckqueue.StateActed:
		return next.State != before.State
	case stuckqueue.StateWaiting, stuckqueue.StateDue, stuckqueue.StateHeld, stuckqueue.StateNotified,
		stuckqueue.StateCleared:
		return smCardChanged(before, next)
	}
	return false
}

func smEventsFor(k stuckqueue.EventKind) []stuckqueue.Event {
	var out []stuckqueue.Event
	for _, at := range smTimes() {
		out = append(out, smEventsAt(k, at)...)
	}
	if k == stuckqueue.EvExpire {
		out = append(out, smKind(k, smAt(40*24*time.Hour)))
	}
	return slices.Compact(out)
}

func smEventsAt(k stuckqueue.EventKind, at time.Time) []stuckqueue.Event {
	switch k {
	case stuckqueue.EvSeen:
		return smSeenEvents(at)
	case stuckqueue.EvMissed, stuckqueue.EvClaimed, stuckqueue.EvClearDone, stuckqueue.EvClearFailed,
		stuckqueue.EvExpire:
		return []stuckqueue.Event{smKind(k, at)}
	case stuckqueue.EvNotifyFailed:
		var out []stuckqueue.Event
		for _, claim := range smTimes() {
			out = append(out, smFailed(at, claim))
		}
		return out
	case stuckqueue.EvActed:
		return smActedEvents(at)
	case stuckqueue.EvImportPosted:
		return smPostedEvents(at)
	case stuckqueue.EvCommandObserved:
		var out []stuckqueue.Event
		for _, c := range stuckqueue.CmdStatuses() {
			out = append(out, smObserved(at, c))
		}
		return out
	case stuckqueue.EvAdopted:
		return []stuckqueue.Event{smOpEv(k, at, 0, false), smOpEv(k, at, 1, false), smOpEv(k, at, 2, false)}
	case stuckqueue.EvResumed:
		var out []stuckqueue.Event
		for _, op := range []int64{0, 1, 2} {
			out = append(out, smOpEv(k, at, op, false), smOpEv(k, at, op, true))
		}
		return out
	}
	return nil
}

func smSeenEvents(at time.Time) []stuckqueue.Event {
	return []stuckqueue.Event{
		smSeen(at, stuckqueue.VerdictClear, false, stuckqueue.OfferRemove),
		smSeen(at, stuckqueue.VerdictClear, true, stuckqueue.OfferRemove),
		smSeen(at, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
		smSeen(at, stuckqueue.VerdictGap, false, stuckqueue.OfferRegrabRemove),
		smSeen(at, stuckqueue.VerdictUnmapped, false, stuckqueue.OfferRemove),
	}
}

func smActedEvents(at time.Time) []stuckqueue.Event {
	none := stuckqueue.Files{}
	out := []stuckqueue.Event{smActedEv(at, stuckqueue.ActionRemove, 0, false, none)}
	for _, op := range []int64{0, 1, 2} {
		out = append(out,
			smActedEv(at, stuckqueue.ActionRegrab, op, false, none),
			smActedEv(at, stuckqueue.ActionRegrab, op, true, none))
	}
	for _, f := range []stuckqueue.Files{{N: 2, M: 2}, {N: 1, M: 2}} {
		out = append(out, smActedEv(at, stuckqueue.ActionImport, 0, false, f))
	}
	return out
}

func smPostedEvents(at time.Time) []stuckqueue.Event {
	var out []stuckqueue.Event
	for _, id := range []int{0, 7} {
		for _, mapped := range []bool{false, true} {
			for _, f := range []stuckqueue.Files{{N: 2, M: 2}, {N: 1, M: 2}} {
				out = append(out, smPosted(at, id, mapped, f))
			}
		}
	}
	return out
}

func smMappedActed(at time.Time, f stuckqueue.Files) stuckqueue.Event {
	e := smActedEv(at, stuckqueue.ActionImport, 0, false, f)
	e.Mapped = true
	return e
}
