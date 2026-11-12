package stuckqueue_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/stuckqueue"
)

type smRow struct {
	id      string
	name    string
	from    stuckqueue.Snapshot
	ev      stuckqueue.Event
	to      stuckqueue.State
	code    stuckqueue.Code
	effects []stuckqueue.Effect
	assert  func(*testing.T, stuckqueue.Snapshot)
}

func smRefresh() []stuckqueue.Effect {
	return []stuckqueue.Effect{stuckqueue.EffRefreshCard{Key: smKey()}}
}

func smClear() stuckqueue.Effect { return stuckqueue.EffClearDownload{Key: smKey()} }

func smActedIs(
	a stuckqueue.Action,
	res stuckqueue.ActResult,
	at time.Time,
	op int64,
) func(*testing.T, stuckqueue.Snapshot) {
	return func(t *testing.T, s stuckqueue.Snapshot) {
		t.Helper()
		got := s.Acted
		if got == nil || got.Action != a || got.Result != res || !got.At.Equal(at) || got.OpID != op {
			t.Errorf("acted = %+v, want %s %s at %s op %d", got, a, res, at, op)
		}
	}
}

func smAssertAll(fs ...func(*testing.T, stuckqueue.Snapshot)) func(*testing.T, stuckqueue.Snapshot) {
	return func(t *testing.T, s stuckqueue.Snapshot) {
		t.Helper()
		for _, f := range fs {
			f(t, s)
		}
	}
}

func smFieldsAre(misses int, fp string, notified time.Time) func(*testing.T, stuckqueue.Snapshot) {
	return func(t *testing.T, s stuckqueue.Snapshot) {
		t.Helper()
		if s.Misses != misses || s.TargetsFP != fp || !s.NotifiedAt.Equal(notified) {
			t.Errorf(
				"misses %d fp %q notified %s, want %d %q %s",
				s.Misses,
				s.TargetsFP,
				s.NotifiedAt,
				misses,
				fp,
				notified,
			)
		}
	}
}

func smWith(s stuckqueue.Snapshot, f func(*stuckqueue.Snapshot)) stuckqueue.Snapshot {
	f(&s)
	return s
}

func smRowsRun() []smRow {
	t0, tw := smAt(0), smAt(2*time.Hour)
	gap := smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll)
	files := stuckqueue.Files{N: 2, M: 2}
	return []smRow{
		{
			id:   "1",
			from: smUntracked(),
			ev:   smSeen(t0, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			to:   stuckqueue.StateWaiting,
			assert: smAssertAll(smFieldsAre(0, "fp", time.Time{}), func(t *testing.T, s stuckqueue.Snapshot) {
				t.Helper()
				if !s.FirstSeen.Equal(t0) || s.Offered != stuckqueue.OfferRemove {
					t.Errorf("first seen %s offered %q", s.FirstSeen, s.Offered)
				}
			}),
		},
		{
			id:      "2",
			from:    smUntracked(),
			ev:      smActedEv(t0, stuckqueue.ActionRemove, 0, false, stuckqueue.Files{}),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionRemove, stuckqueue.ResultOK, t0, 0),
		},
		{id: "3", from: smUntracked(), ev: smKind(stuckqueue.EvMissed, t0), code: stuckqueue.CodeUntracked},
		{
			id:   "3",
			name: "regrab",
			from: smUntracked(),
			ev:   smActedEv(t0, stuckqueue.ActionRegrab, 1, false, stuckqueue.Files{}),
			code: stuckqueue.CodeUntracked,
		},
		{
			id:   "3",
			name: "posted",
			from: smUntracked(),
			ev:   smPosted(t0, 7, false, files),
			code: stuckqueue.CodeUntracked,
		},
		{
			id:      "4",
			from:    smWith(smWaiting(), func(s *stuckqueue.Snapshot) { s.Misses = 1 }),
			ev:      gap,
			to:      stuckqueue.StateDue,
			effects: smRefresh(),
			assert:  smFieldsAre(0, "fp", time.Time{}),
		},
		{
			id:      "4",
			name:    "held clears",
			from:    smDue(),
			ev:      smSeen(tw, stuckqueue.VerdictClear, false, stuckqueue.OfferRemove),
			to:      stuckqueue.StateHeld,
			effects: []stuckqueue.Effect{smClear(), smRefresh()[0]},
		},
		{
			id:      "4",
			name:    "hold flag",
			from:    smDue(),
			ev:      smSeen(tw, stuckqueue.VerdictClear, true, stuckqueue.OfferRemove),
			to:      stuckqueue.StateHeld,
			effects: smRefresh(),
		},
		{id: "4", name: "no change", from: smDue(), ev: gap, to: stuckqueue.StateDue},
		{id: "5", from: smNotified(), ev: smKind(stuckqueue.EvMissed, tw), to: stuckqueue.StateNotified,
			assert: smFieldsAre(1, "fp", t0)},
		{
			id:      "6",
			from:    smWith(smNotified(), func(s *stuckqueue.Snapshot) { s.Misses = 1 }),
			ev:      smKind(stuckqueue.EvMissed, tw),
			to:      stuckqueue.StateUntracked,
			effects: smRefresh(),
		},
		{id: "7", from: smDue(), ev: smKind(stuckqueue.EvClaimed, tw), to: stuckqueue.StateNotified,
			assert: smFieldsAre(0, "fp", tw)},
		{id: "8", from: smHeld(), ev: smKind(stuckqueue.EvClaimed, tw), code: stuckqueue.CodeNotDue},
		{
			id:     "9",
			from:   smNotified(),
			ev:     smFailed(tw, t0),
			to:     stuckqueue.StateDue,
			assert: smFieldsAre(0, "fp", time.Time{}),
		},
		{id: "10", from: smNotified(), ev: smFailed(tw, tw), code: stuckqueue.CodeStaleClaim},
		{id: "11", from: smHeld(), ev: smFailed(tw, t0), code: stuckqueue.CodeNotNotified},
		{id: "12", from: smHeld(), ev: smKind(stuckqueue.EvClearDone, tw), to: stuckqueue.StateCleared},
		{id: "13", from: smHeld(), ev: smKind(stuckqueue.EvClearFailed, tw), to: stuckqueue.StateHeld},
		{id: "14", from: smNotified(), ev: smKind(stuckqueue.EvClearDone, tw), code: stuckqueue.CodeNotClearing},
		{
			id:   "14",
			name: "failed",
			from: smCleared(),
			ev:   smKind(stuckqueue.EvClearFailed, tw),
			code: stuckqueue.CodeNotClearing,
		},
	}
}

func smRowsOwner() []smRow {
	t0, tw := smAt(0), smAt(2*time.Hour)
	files := stuckqueue.Files{N: 2, M: 2}
	none := stuckqueue.Files{}
	return []smRow{
		{
			id:      "15",
			from:    smWaiting(),
			ev:      smActedEv(t0, stuckqueue.ActionRemove, 0, false, none),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionRemove, stuckqueue.ResultOK, t0, 0),
		},
		{
			id:      "16",
			from:    smNotified(),
			ev:      smActedEv(tw, stuckqueue.ActionRegrab, 5, true, none),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionRegrab, stuckqueue.ResultUnfinished, tw, 5),
		},
		{
			id:      "16",
			name:    "done",
			from:    smNotified(),
			ev:      smActedEv(tw, stuckqueue.ActionRegrab, 5, false, none),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionRegrab, stuckqueue.ResultOK, tw, 5),
		},
		{
			id:   "16a",
			from: smNotified(),
			ev:   smActedEv(tw, stuckqueue.ActionRegrab, 0, true, none),
			code: stuckqueue.CodeNoOp,
		},
		{
			id:      "17",
			from:    smHeld(),
			ev:      smActedEv(tw, stuckqueue.ActionImport, 0, false, files),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert: func(t *testing.T, s stuckqueue.Snapshot) {
				t.Helper()
				smActedIs(stuckqueue.ActionImport, stuckqueue.ResultOK, tw, 0)(t, s)
				if s.Acted.Files != files {
					t.Errorf("files = %+v", s.Acted.Files)
				}
			},
		},
		{
			id:   "18",
			from: smWaiting(),
			ev:   smActedEv(t0, stuckqueue.ActionRegrab, 1, false, none),
			code: stuckqueue.CodeInsideWait,
		},
		{id: "18", name: "import", from: smWaiting(), ev: smActedEv(t0, stuckqueue.ActionImport, 0, false, files),
			code: stuckqueue.CodeInsideWait},
		{
			id:   "18",
			name: "posted",
			from: smWaiting(),
			ev:   smPosted(t0, 7, true, files),
			code: stuckqueue.CodeInsideWait,
		},
		{
			id:      "19",
			from:    smDue(),
			ev:      smPosted(tw, 7, false, files),
			to:      stuckqueue.StateImportInflight,
			effects: smRefresh(),
			assert: func(t *testing.T, s stuckqueue.Snapshot) {
				t.Helper()
				if s.Import == nil || *s.Import != (stuckqueue.ImportCmd{CommandID: 7, StartedAt: tw, Files: files}) ||
					s.Offered != stuckqueue.OfferNone {
					t.Errorf("import = %+v offered %q", s.Import, s.Offered)
				}
			},
		},
		{id: "19", name: "past the wall clock", from: smWaiting(), ev: smPosted(tw, 7, false, files),
			to: stuckqueue.StateImportInflight, effects: smRefresh()},
		{id: "20", from: smDue(), ev: smPosted(tw, 0, false, files), code: stuckqueue.CodeNoCommand},
		{id: "21", from: smHeld(), ev: smObserved(tw, stuckqueue.CmdCompleted), code: stuckqueue.CodeNoImport},
		{id: "22", from: smHeld(), ev: smKind(stuckqueue.EvExpire, tw), code: stuckqueue.CodeNotActed},
	}
}

func smRowsInflight() []smRow {
	t0, tw, tg := smAt(0), smAt(2*time.Hour), smAt(11*time.Minute)
	files := stuckqueue.Files{N: 2, M: 2}
	return []smRow{
		{id: "23", from: smInflight(), ev: smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
			to: stuckqueue.StateImportInflight, assert: smFieldsAre(0, "fp", time.Time{})},
		{id: "24", from: smInflight(), ev: smKind(stuckqueue.EvMissed, tw), to: stuckqueue.StateImportInflight},
		{
			id:      "25",
			from:    smInflight(),
			ev:      smObserved(tg, stuckqueue.CmdCompleted),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert: func(t *testing.T, s stuckqueue.Snapshot) {
				t.Helper()
				smActedIs(stuckqueue.ActionImport, stuckqueue.ResultOK, tg, 0)(t, s)
				if s.Import != nil || s.Acted.Files != files {
					t.Errorf("import %+v files %+v", s.Import, s.Acted.Files)
				}
			},
		},
		{
			id:      "25a",
			name:    "gone",
			from:    smInflight(),
			ev:      smObserved(tg, stuckqueue.CmdGone),
			to:      stuckqueue.StateNotified,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionImport, stuckqueue.ResultImportUnconfirmed, tg, 0),
		},
		{
			id:      "26",
			from:    smInflight(),
			ev:      smObserved(tg, stuckqueue.CmdFailed),
			to:      stuckqueue.StateNotified,
			effects: smRefresh(),
			assert: smAssertAll(
				smFieldsAre(0, "", tg),
				smActedIs(stuckqueue.ActionImport, stuckqueue.ResultImportFailed, tg, 0),
			),
		},
		{
			id:      "26",
			name:    "keeps the first notice",
			from:    smWith(smInflight(), func(s *stuckqueue.Snapshot) { s.NotifiedAt = t0 }),
			ev:      smObserved(tg, stuckqueue.CmdFailed),
			to:      stuckqueue.StateNotified,
			effects: smRefresh(),
			assert:  smFieldsAre(0, "", t0),
		},
		{
			id:      "27",
			from:    smInflight(),
			ev:      smObserved(tw, stuckqueue.CmdRunning),
			to:      stuckqueue.StateNotified,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionImport, stuckqueue.ResultImportTimeout, tw, 0),
		},
		{
			id:      "28",
			from:    smInflight(),
			ev:      smObserved(tw, stuckqueue.CmdUnreadable),
			to:      stuckqueue.StateNotified,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionImport, stuckqueue.ResultImportUnconfirmed, tw, 0),
		},
		{id: "29", from: smInflight(), ev: smObserved(tg, stuckqueue.CmdRunning), to: stuckqueue.StateImportInflight},
		{id: "29", name: "unreadable", from: smInflight(), ev: smObserved(tg, stuckqueue.CmdUnreadable),
			to: stuckqueue.StateImportInflight},
		{id: "30", from: smInflight(), ev: smKind(stuckqueue.EvClaimed, tw), code: stuckqueue.CodeImportRunning},
		{
			id:   "30",
			name: "acted",
			from: smInflight(),
			ev:   smActedEv(tw, stuckqueue.ActionRemove, 0, false, stuckqueue.Files{}),
			code: stuckqueue.CodeImportRunning,
		},
		{
			id:   "30",
			name: "posted",
			from: smInflight(),
			ev:   smPosted(tw, 8, false, files),
			code: stuckqueue.CodeImportRunning,
		},
		{
			id:   "30",
			name: "expire",
			from: smInflight(),
			ev:   smKind(stuckqueue.EvExpire, tw),
			code: stuckqueue.CodeImportRunning,
		},
	}
}

func smRowsActed() []smRow {
	t0, tw, tg, tk := smAt(0), smAt(2*time.Hour), smAt(11*time.Minute), smAt(17*24*time.Hour)
	gap := func(at time.Time) stuckqueue.Event {
		return smSeen(at, stuckqueue.VerdictGap, false, stuckqueue.OfferAll)
	}
	files := stuckqueue.Files{N: 2, M: 2}
	return []smRow{
		{id: "31", from: smActedRemove(), ev: gap(smAt(5 * time.Minute)), to: stuckqueue.StateActed},
		{id: "32", from: smActedRemove(), ev: gap(tw), to: stuckqueue.StateNotified, effects: smRefresh(),
			assert: smAssertAll(smFieldsAre(0, "fp", tw), func(t *testing.T, s stuckqueue.Snapshot) {
				t.Helper()
				if s.Acted != nil || s.Offered != stuckqueue.OfferAll {
					t.Errorf("acted %+v offered %q", s.Acted, s.Offered)
				}
			})},
		{
			id:      "32",
			name:    "regrab ok",
			from:    smActedRegrab(stuckqueue.ResultOK, 3),
			ev:      gap(tw),
			to:      stuckqueue.StateNotified,
			effects: smRefresh(),
		},
		{
			id:      "33",
			from:    smActedImport(2, 2),
			ev:      gap(tw),
			to:      stuckqueue.StateNotified,
			effects: smRefresh(),
			assert: smAssertAll(
				smFieldsAre(0, "fp", tw),
				smActedIs(stuckqueue.ActionImport, stuckqueue.ResultImportFailed, t0, 0),
			),
		},
		{id: "34", from: smActedImport(1, 2), ev: gap(tw), to: stuckqueue.StateNotified, effects: smRefresh(),
			assert: func(t *testing.T, s stuckqueue.Snapshot) {
				t.Helper()
				smActedIs(stuckqueue.ActionImport, stuckqueue.ResultPartial, t0, 0)(t, s)
				if s.Acted.Files != (stuckqueue.Files{N: 1, M: 2}) {
					t.Errorf("files = %+v", s.Acted.Files)
				}
			}},
		{
			id:     "35",
			from:   smActedRemove(),
			ev:     smKind(stuckqueue.EvMissed, tw),
			to:     stuckqueue.StateActed,
			assert: smFieldsAre(0, "", time.Time{}),
		},
		{id: "36", from: smActedRemove(), ev: smKind(stuckqueue.EvExpire, tk), to: stuckqueue.StateUntracked},
		{id: "37", from: smActedRemove(), ev: smKind(stuckqueue.EvExpire, tg), code: stuckqueue.CodeNotExpired},
		{id: "38", from: smActedImport(1, 2), ev: smPosted(tg, 9, true, files), to: stuckqueue.StateImportInflight,
			effects: smRefresh(), assert: func(t *testing.T, s stuckqueue.Snapshot) {
				t.Helper()
				if s.Import == nil || s.Import.CommandID != 9 || s.Acted != nil {
					t.Errorf("import %+v acted %+v", s.Import, s.Acted)
				}
			}},
		{
			id:      "38a",
			from:    smActedImport(1, 2),
			ev:      smMappedActed(tg, files),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert:  smActedIs(stuckqueue.ActionImport, stuckqueue.ResultOK, tg, 0),
		},
		{id: "39", from: smActedRemove(), ev: smKind(stuckqueue.EvClaimed, tg), code: stuckqueue.CodeAlreadyDone},
		{
			id:   "39a",
			name: "plain import",
			from: smActedImport(1, 2),
			ev:   smPosted(tg, 9, false, files),
			code: stuckqueue.CodeAlreadyDone,
		},
		{
			id:   "39a",
			name: "mapped on remove",
			from: smActedRemove(),
			ev:   smPosted(tg, 9, true, files),
			code: stuckqueue.CodeAlreadyDone,
		},
		{
			id:   "39a",
			name: "no command",
			from: smActedImport(1, 2),
			ev:   smPosted(tg, 0, true, files),
			code: stuckqueue.CodeAlreadyDone,
		},
		{
			id:   "39",
			name: "observed",
			from: smActedImport(1, 2),
			ev:   smObserved(tg, stuckqueue.CmdCompleted),
			code: stuckqueue.CodeAlreadyDone,
		},
		{
			id:   "40",
			from: smActedRegrab(stuckqueue.ResultUnfinished, 3),
			ev:   gap(tw),
			to:   stuckqueue.StateActed,
			assert: smAssertAll(
				smFieldsAre(0, "fp", time.Time{}),
				smActedIs(stuckqueue.ActionRegrab, stuckqueue.ResultUnfinished, t0, 3),
			),
		},
	}
}

func smRowsOps() []smRow {
	tw, tg := smAt(2*time.Hour), smAt(11*time.Minute)
	unfinished := smActedRegrab(stuckqueue.ResultUnfinished, 4)
	adopt := func(op int64) stuckqueue.Event { return smOpEv(stuckqueue.EvAdopted, tg, op, false) }
	resume := func(op int64, done bool) stuckqueue.Event { return smOpEv(stuckqueue.EvResumed, tg, op, done) }
	return []smRow{
		{
			id:      "41",
			from:    smUntracked(),
			ev:      adopt(4),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert: smAssertAll(
				smActedIs(stuckqueue.ActionRegrab, stuckqueue.ResultUnfinished, tg, 4),
				func(t *testing.T, s stuckqueue.Snapshot) {
					t.Helper()
					if !s.FirstSeen.Equal(tg) || s.Offered != stuckqueue.OfferNone {
						t.Errorf("first seen %s offered %q", s.FirstSeen, s.Offered)
					}
				},
			),
		},
		{
			id:      "42",
			from:    smWith(smNotified(), func(s *stuckqueue.Snapshot) { s.Misses = 1 }),
			ev:      smOpEv(stuckqueue.EvAdopted, tw, 4, false),
			to:      stuckqueue.StateActed,
			effects: smRefresh(),
			assert: smAssertAll(
				smActedIs(stuckqueue.ActionRegrab, stuckqueue.ResultUnfinished, tw, 4),
				smFieldsAre(0, "fp", smAt(0)),
			),
		},
		{id: "43", from: unfinished, ev: adopt(4), code: stuckqueue.CodeAdopted},
		{id: "44", from: unfinished, ev: adopt(5), code: stuckqueue.CodeAlreadyDone},
		{id: "45", from: smInflight(), ev: adopt(4), code: stuckqueue.CodeImportRunning},
		{id: "46", from: unfinished, ev: resume(4, true), to: stuckqueue.StateActed, effects: smRefresh(),
			assert: smActedIs(stuckqueue.ActionRegrab, stuckqueue.ResultOK, tg, 4)},
		{id: "47", from: unfinished, ev: resume(4, false), to: stuckqueue.StateActed, effects: smRefresh(),
			assert: smActedIs(stuckqueue.ActionRegrab, stuckqueue.ResultUnfinished, tg, 4)},
		{id: "48", from: unfinished, ev: resume(5, true), code: stuckqueue.CodeOpChanged},
		{id: "49", from: smActedRemove(), ev: resume(4, true), code: stuckqueue.CodeNotResumable},
		{id: "49", name: "import", from: smActedImport(2, 2), ev: resume(4, false), code: stuckqueue.CodeNotResumable},
		{
			id:   "49",
			name: "regrab ok",
			from: smActedRegrab(stuckqueue.ResultOK, 4),
			ev:   resume(4, true),
			code: stuckqueue.CodeNotResumable,
		},
		{id: "50", from: smUntracked(), ev: resume(4, true), code: stuckqueue.CodeNotResumable},
		{id: "50", name: "tracked", from: smHeld(), ev: resume(4, true), code: stuckqueue.CodeNotResumable},
		{id: "50", name: "inflight", from: smInflight(), ev: resume(4, true), code: stuckqueue.CodeNotResumable},
		{id: "51", from: smUntracked(), ev: adopt(0), code: stuckqueue.CodeNoOp},
		{id: "51", name: "resume", from: unfinished, ev: resume(0, true), code: stuckqueue.CodeNoOp},
		{id: "51", name: "inflight", from: smInflight(), ev: adopt(0), code: stuckqueue.CodeNoOp},
	}
}

func smAllRows() []smRow {
	return slices.Concat(smRowsRun(), smRowsOwner(), smRowsInflight(), smRowsActed(), smRowsOps())
}

func TestStuckMachine_TableRows(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range smAllRows() {
		seen[c.id] = true
		t.Run(strings.TrimSpace(c.id+" "+c.name), func(t *testing.T) {
			t.Parallel()
			if got := stuckqueue.RowID(c.from, c.ev); got != c.id {
				t.Fatalf("row = %q, want %q", got, c.id)
			}
			if n := stuckqueue.Matches(c.from, c.ev); n != 1 {
				t.Fatalf("%d rows match, want 1", n)
			}
			smCheckRow(t, c)
		})
	}
	for _, id := range []string{"16a", "51"} {
		if !seen[id] {
			t.Errorf("row %s has no case", id)
		}
	}
	for i := 1; i <= 51; i++ {
		if id := strconv.Itoa(i); !seen[id] {
			t.Errorf("row %s has no case", id)
		}
	}
}

func TestStuckMachine_Row3RejectsEveryKind(t *testing.T) {
	t.Parallel()
	for _, k := range []stuckqueue.EventKind{
		stuckqueue.EvMissed, stuckqueue.EvClaimed, stuckqueue.EvNotifyFailed, stuckqueue.EvClearDone,
		stuckqueue.EvClearFailed, stuckqueue.EvImportPosted, stuckqueue.EvCommandObserved, stuckqueue.EvExpire,
	} {
		for _, e := range smEventsFor(k) {
			_, _, err := stuckqueue.Next(smUntracked(), e)
			var rej *stuckqueue.RejectError
			if !errors.As(err, &rej) || rej.Code != stuckqueue.CodeUntracked {
				t.Errorf("%+v: err = %v, want untracked", e, err)
			}
		}
	}
}

func TestQ1_11_OneMissKeepsRow(t *testing.T) {
	t.Parallel()
	for _, s := range smAll() {
		if s.Misses != 0 || s.State == stuckqueue.StateUntracked {
			continue
		}
		next, effs, err := stuckqueue.Next(s, smKind(stuckqueue.EvMissed, smAt(2*time.Hour)))
		if err != nil || next.State != s.State || len(effs) != 0 {
			t.Errorf("%s: one miss gave %s %v %v", smKeyOf(s), next.State, effs, err)
		}
		tracked := s.State != stuckqueue.StateActed && s.State != stuckqueue.StateImportInflight
		if tracked && next.Misses != 1 {
			t.Errorf("%s: misses = %d, want 1", smKeyOf(s), next.Misses)
		}
		if !tracked && next.Misses != 0 {
			t.Errorf("%s: misses = %d, want 0", smKeyOf(s), next.Misses)
		}
		back, _, err := stuckqueue.Next(
			next,
			smSeen(smAt(2*time.Hour), stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
		)
		if err != nil || back.Misses != 0 {
			t.Errorf("%s: a sighting left misses %d (%v)", smKeyOf(s), back.Misses, err)
		}
	}
}

func TestQ1_11_SecondMissPrunes(t *testing.T) {
	t.Parallel()
	for _, s := range smAll() {
		if s.Misses != 1 {
			continue
		}
		next, effs, err := stuckqueue.Next(s, smKind(stuckqueue.EvMissed, smAt(2*time.Hour)))
		if err != nil || next.State != stuckqueue.StateUntracked || !slices.Equal(effs, smRefresh()) {
			t.Errorf("%s: second miss gave %s %v %v", smKeyOf(s), next.State, effs, err)
		}
	}
}

func TestF24_GoneCommandIsUnconfirmed(t *testing.T) {
	t.Parallel()
	next, _, err := stuckqueue.Next(smInflight(), smObserved(smAt(11*time.Minute), stuckqueue.CmdGone))
	if err != nil || next.State != stuckqueue.StateNotified || next.Acted == nil ||
		next.Acted.Result != stuckqueue.ResultImportUnconfirmed {
		t.Errorf("gone gave %+v, %v", next, err)
	}
}

func TestCompletedImportStillStuckIsFailed(t *testing.T) {
	t.Parallel()
	tg, tw := smAt(11*time.Minute), smAt(2*time.Hour)
	acted, _, err := stuckqueue.Next(smInflight(), smObserved(tg, stuckqueue.CmdCompleted))
	if err != nil || acted.State != stuckqueue.StateActed {
		t.Fatalf("completed gave %s, %v", acted.State, err)
	}
	next, effs, err := stuckqueue.Next(acted, smSeen(tw, stuckqueue.VerdictGap, false, stuckqueue.OfferAll))
	if err != nil || next.State != stuckqueue.StateNotified || next.Acted == nil ||
		next.Acted.Result != stuckqueue.ResultImportFailed || next.Offered != stuckqueue.OfferAll ||
		next.NotifiedAt.IsZero() || !slices.Equal(effs, smRefresh()) {
		t.Errorf("still stuck gave %+v %v %v", next, effs, err)
	}
}

func TestPartialImportReturnsWithCount(t *testing.T) {
	t.Parallel()
	next, _, err := stuckqueue.Next(
		smActedImport(1, 3),
		smSeen(smAt(2*time.Hour), stuckqueue.VerdictGap, false, stuckqueue.OfferAll),
	)
	if err != nil || next.State != stuckqueue.StateNotified || next.Acted == nil ||
		next.Acted.Result != stuckqueue.ResultPartial || next.Acted.Files != (stuckqueue.Files{N: 1, M: 3}) {
		t.Errorf("partial import gave %+v %v", next, err)
	}
}

func TestRow32_UnfinishedSurvivesGrace(t *testing.T) {
	t.Parallel()
	s := smActedRegrab(stuckqueue.ResultUnfinished, 3)
	for _, at := range []time.Time{smAt(11 * time.Minute), smAt(2 * time.Hour), smAt(17 * 24 * time.Hour)} {
		for _, v := range []stuckqueue.Verdict{stuckqueue.VerdictGap, stuckqueue.VerdictClear} {
			next, effs, err := stuckqueue.Next(s, smSeen(at, v, false, stuckqueue.OfferAll))
			if err != nil || next.State != stuckqueue.StateActed || next.Acted.Result != stuckqueue.ResultUnfinished ||
				next.Acted.OpID != 3 || len(effs) != 0 {
				t.Errorf("at %s %s: %+v %v %v", at, v, next, effs, err)
			}
		}
	}
}

func TestEvAdopted_Rows(t *testing.T) {
	t.Parallel()
	want := map[string]stuckqueue.Code{
		"untracked": "", "waiting": "", "due": "", "held": "", "notified": "", "cleared": "",
		"inflight": stuckqueue.CodeImportRunning, "unfinished same": stuckqueue.CodeAdopted,
		"remove": stuckqueue.CodeAlreadyDone,
	}
	snaps := map[string]stuckqueue.Snapshot{
		"untracked":       smUntracked(),
		"waiting":         smWaiting(),
		"due":             smDue(),
		"held":            smHeld(),
		"notified":        smNotified(),
		"cleared":         smCleared(),
		"inflight":        smInflight(),
		"unfinished same": smActedRegrab(stuckqueue.ResultUnfinished, 4),
		"remove":          smActedRemove(),
	}
	for name, s := range snaps {
		_, _, err := stuckqueue.Next(s, smOpEv(stuckqueue.EvAdopted, smAt(2*time.Hour), 4, false))
		var rej *stuckqueue.RejectError
		switch {
		case want[name] == "" && err != nil:
			t.Errorf("%s: adopted rejected: %v", name, err)
		case want[name] != "" && (!errors.As(err, &rej) || rej.Code != want[name]):
			t.Errorf("%s: err = %v, want %s", name, err, want[name])
		}
	}
	_, _, err := stuckqueue.Next(
		smActedRegrab(stuckqueue.ResultUnfinished, 4),
		smOpEv(stuckqueue.EvAdopted, smAt(2*time.Hour), 5, false),
	)
	var rej *stuckqueue.RejectError
	if !errors.As(err, &rej) || rej.Code != stuckqueue.CodeAlreadyDone || !strings.Contains(rej.Note, "5") {
		t.Errorf("other op: err = %v, want already_done naming op 5", err)
	}
}

func TestEvResumed_Rows(t *testing.T) {
	t.Parallel()
	at := smAt(2 * time.Hour)
	unfinished := smActedRegrab(stuckqueue.ResultUnfinished, 4)
	for _, done := range []bool{true, false} {
		next, _, err := stuckqueue.Next(unfinished, smOpEv(stuckqueue.EvResumed, at, 4, done))
		want := stuckqueue.ResultUnfinished
		if done {
			want = stuckqueue.ResultOK
		}
		if err != nil || next.Acted.Result != want || !next.Acted.At.Equal(at) || next.Acted.OpID != 4 {
			t.Errorf("done=%v: %+v %v", done, next.Acted, err)
		}
	}
	for _, s := range smAll() {
		for _, op := range []int64{0, 4} {
			smResumeOutcome(t, s, op)
		}
	}
}

func TestActed_RegrabCarriesOpID(t *testing.T) {
	t.Parallel()
	at := smAt(2 * time.Hour)
	for _, resumable := range []bool{true, false} {
		next, _, err := stuckqueue.Next(
			smNotified(),
			smActedEv(at, stuckqueue.ActionRegrab, 9, resumable, stuckqueue.Files{}),
		)
		want := stuckqueue.ResultOK
		if resumable {
			want = stuckqueue.ResultUnfinished
		}
		if err != nil || next.Acted == nil || next.Acted.OpID != 9 || next.Acted.Result != want {
			t.Errorf("resumable=%v: %+v %v", resumable, next.Acted, err)
		}
		_, _, err = stuckqueue.Next(
			smNotified(),
			smActedEv(at, stuckqueue.ActionRegrab, 0, resumable, stuckqueue.Files{}),
		)
		var rej *stuckqueue.RejectError
		if !errors.As(err, &rej) || rej.Code != stuckqueue.CodeNoOp {
			t.Errorf("op 0 resumable=%v: %v", resumable, err)
		}
	}
}

func TestCheck_C10OneWay(t *testing.T) {
	t.Parallel()
	if err := stuckqueue.Check(smActedRegrab(stuckqueue.ResultUnfinished, 0)); err == nil {
		t.Error("unfinished without an op id passed")
	}
	rm := smActedRemove()
	rm.Acted.OpID = 3
	if err := stuckqueue.Check(rm); err == nil {
		t.Error("an op id on remove passed")
	}
	if err := stuckqueue.Check(smActedRegrab(stuckqueue.ResultOK, 3)); err != nil {
		t.Errorf("acted regrab ok with an op id: %v", err)
	}
	if err := stuckqueue.Check(smActedRegrab(stuckqueue.ResultOK, 0)); err != nil {
		t.Errorf("acted regrab ok without an op id: %v", err)
	}
}

func smCheckRow(t *testing.T, c smRow) {
	t.Helper()
	next, effs, err := stuckqueue.Next(c.from, c.ev)
	if c.code != "" {
		var rej *stuckqueue.RejectError
		if !errors.As(err, &rej) || rej.Code != c.code {
			t.Fatalf("err = %v, want reject %s", err, c.code)
		}
		if smKeyOf(next) != smKeyOf(c.from) || len(effs) != 0 {
			t.Errorf("reject changed the snapshot or emitted %v", effs)
		}
		return
	}
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if next.State != c.to {
		t.Fatalf("state = %s, want %s", next.State, c.to)
	}
	if !slices.Equal(effs, c.effects) {
		t.Errorf("effects = %v, want %v", effs, c.effects)
	}
	if cerr := stuckqueue.Check(next); cerr != nil {
		t.Errorf("result breaks an invariant: %v", cerr)
	}
	if c.assert != nil {
		c.assert(t, next)
	}
}

func smResumeOutcome(t *testing.T, s stuckqueue.Snapshot, op int64) {
	t.Helper()
	_, _, err := stuckqueue.Next(s, smOpEv(stuckqueue.EvResumed, smAt(2*time.Hour), op, true))
	var rej *stuckqueue.RejectError
	switch {
	case op == 0:
		if !errors.As(err, &rej) || rej.Code != stuckqueue.CodeNoOp {
			t.Errorf("%s op 0: %v", smKeyOf(s), err)
		}
	case s.State == stuckqueue.StateActed && s.Acted.Result == stuckqueue.ResultUnfinished:
	default:
		if !errors.As(err, &rej) || rej.Code != stuckqueue.CodeNotResumable {
			t.Errorf("%s op 4: %v", smKeyOf(s), err)
		}
	}
}

func TestF26_RowIDsAreUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, id := range stuckqueue.TransitionIDsForTest() {
		if seen[id] {
			t.Errorf("row id %q is used twice", id)
		}
		seen[id] = true
	}
}
