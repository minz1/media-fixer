package sweepstate_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

type itemMach = machinetest.Machine[sweepstate.ItemResult, sweepstate.ItemEventKind, sweepstate.ItemSnap, sweepstate.ItemEvent]

func itemStates() []sweepstate.ItemResult {
	return append([]sweepstate.ItemResult{sweepstate.ItemNone}, sweepstate.ItemResults()...)
}

func itemShapes() []sweepstate.ItemSnap {
	var out []sweepstate.ItemSnap
	for _, dry := range []bool{false, true} {
		for _, rel := range []string{"", "rel"} {
			for _, eps := range []string{"", "eps"} {
				out = append(out, sweepstate.ItemSnap{Dry: dry, ReleaseTitle: rel, Episodes: eps})
			}
		}
	}
	return out
}

func itemSamples(r sweepstate.ItemResult) []sweepstate.ItemSnap {
	if r == sweepstate.ItemNone {
		return []sweepstate.ItemSnap{{Result: r}}
	}
	var out []sweepstate.ItemSnap
	for _, s := range itemShapes() {
		if sweepstate.CheckItem(r, s.Dry, s.ReleaseTitle, s.Episodes) == nil {
			s.Result = r
			out = append(out, s)
		}
	}
	return out
}

func itemEvents(k sweepstate.ItemEventKind) []sweepstate.ItemEvent {
	var out []sweepstate.ItemEvent
	for _, to := range itemStates() {
		if k == sweepstate.EvMoved {
			out = append(out, sweepstate.ItemEvent{Kind: k, To: to})
			continue
		}
		for _, s := range itemShapes() {
			out = append(out, sweepstate.ItemEvent{
				Kind: k, To: to, Dry: s.Dry, ReleaseTitle: s.ReleaseTitle, Episodes: s.Episodes,
			})
		}
	}
	return out
}

func itemMachine() itemMach {
	return itemMach{
		States:  itemStates(),
		Kinds:   []sweepstate.ItemEventKind{sweepstate.EvInserted, sweepstate.EvMoved},
		Initial: sweepstate.ItemSnap{Result: sweepstate.ItemNone},
		State:   func(s sweepstate.ItemSnap) sweepstate.ItemResult { return s.Result },
		Key:     func(s sweepstate.ItemSnap) string { return fmt.Sprintf("%+v", s) },
		Samples: itemSamples,
		Events:  itemEvents,
		Matches: sweepstate.ItemRowsFor,
		Next:    sweepstate.NextItem,
		Check: func(s sweepstate.ItemSnap) error {
			return sweepstate.CheckItem(s.Result, s.Dry, s.ReleaseTitle, s.Episodes)
		},
		Terminal: func(r sweepstate.ItemResult) bool {
			return r != sweepstate.ItemNone && r != sweepstate.Grabbing
		},
	}
}

func TestItemMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, itemMachine())
}

func TestItemMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, itemMachine())
	none := sweepstate.ItemSnap{Result: sweepstate.ItemNone}
	first := make(map[sweepstate.ItemResult]bool)
	for _, ev := range itemEvents(sweepstate.EvInserted) {
		if next, err := sweepstate.NextItem(none, ev); err == nil {
			first[next.Result] = true
		}
	}
	initial := sweepstate.ItemsWhere(func(m sweepstate.ItemMeta) bool { return m.Initial })
	if len(initial) != 6 || len(first) != 6 {
		t.Errorf("initial = %v, reached from none = %v, want six", initial, first)
	}
	for _, r := range initial {
		if !first[r] {
			t.Errorf("%s is not reachable from none", r)
		}
	}
	for _, to := range []sweepstate.ItemResult{
		sweepstate.Imported, sweepstate.ItemFailed, sweepstate.Downloading, sweepstate.Archive, sweepstate.ItemRefused,
	} {
		if err := sweepstate.Move(sweepstate.Grabbing, to); err != nil {
			t.Errorf("grabbing -> %s: %v", to, err)
		}
	}
}

func TestItemMachine_Invariants(t *testing.T) {
	t.Parallel()
	machinetest.Invariants(t, itemMachine())
}

func TestItemMachine_FailedInsertLeavesNone(t *testing.T) {
	t.Parallel()
	none := sweepstate.ItemSnap{Result: sweepstate.ItemNone}
	bad := []sweepstate.ItemEvent{
		{Kind: sweepstate.EvInserted, To: sweepstate.Grabbing},
		{Kind: sweepstate.EvInserted, To: sweepstate.WouldGrab, ReleaseTitle: "r"},
		{Kind: sweepstate.EvInserted, To: sweepstate.Searched, Dry: false},
		{Kind: sweepstate.EvInserted, To: sweepstate.Imported, ReleaseTitle: "r"},
		{Kind: sweepstate.EvInserted, To: sweepstate.ItemNone},
	}
	for _, ev := range bad {
		next, err := sweepstate.NextItem(none, ev)
		if err == nil || next != none {
			t.Errorf("%+v: got %+v, %v, want a rejection leaving none", ev, next, err)
		}
	}
}

func TestItemMachine_Moves(t *testing.T) {
	t.Parallel()
	ok := map[[2]sweepstate.ItemResult]bool{
		{sweepstate.Grabbing, sweepstate.Imported}:    true,
		{sweepstate.Grabbing, sweepstate.ItemFailed}:  true,
		{sweepstate.Grabbing, sweepstate.Downloading}: true,
		{sweepstate.Grabbing, sweepstate.Archive}:     true,
		{sweepstate.Grabbing, sweepstate.ItemRefused}: true,
		{sweepstate.Searched, sweepstate.ItemFailed}:  true,
	}
	for _, from := range itemStates() {
		for _, to := range itemStates() {
			err := sweepstate.Move(from, to)
			if want := ok[[2]sweepstate.ItemResult{from, to}]; (err == nil) != want {
				t.Errorf("Move(%q, %q) = %v, want ok = %v", from, to, err, want)
			}
		}
	}
}

func TestItemMachine_InsertedTwiceRejected(t *testing.T) {
	t.Parallel()
	for _, r := range sweepstate.ItemResults() {
		cur := sweepstate.ItemSnap{Result: r}
		ev := sweepstate.ItemEvent{Kind: sweepstate.EvInserted, To: sweepstate.NoPack}
		if next, err := sweepstate.NextItem(cur, ev); err == nil || next != cur {
			t.Errorf("%s: second insert got %+v, %v", r, next, err)
		}
	}
}

func FuzzItemMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	f.Add([]byte{12, 88, 90, 91, 92, 93})
	f.Add([]byte{255, 0, 3, 77, 100, 96, 97})
	m := itemMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func TestS3_Copies_GrabSetUnchanged(t *testing.T) {
	t.Parallel()
	got := slices.Clone(sweepstate.ItemsWhere(func(m sweepstate.ItemMeta) bool { return m.Grab }))
	slices.Sort(got)
	want := []sweepstate.ItemResult{"grabbing", "imported", "failed", "refused", "archive", "downloading"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Grab = %v, want %v", got, want)
	}
	if n := len(sweepstate.ItemResults()); n != 10 {
		t.Errorf("%d stored results, want 10", n)
	}
	for _, r := range sweepstate.ItemResults() {
		if r == sweepstate.ItemNone {
			t.Error("ItemNone listed as stored")
		}
	}
}

func TestCheckItem_Table(t *testing.T) {
	t.Parallel()
	type c struct {
		r        sweepstate.ItemResult
		dry      bool
		rel, eps string
		ok       bool
	}
	cases := []c{
		{sweepstate.Grabbing, false, "r", "", true},
		{sweepstate.Grabbing, false, "", "", false},
		{sweepstate.Grabbing, true, "r", "", false},
		{sweepstate.Imported, false, "r", "e", true},
		{sweepstate.Imported, false, "", "", false},
		{sweepstate.Downloading, false, "r", "", true},
		{sweepstate.Archive, true, "r", "", false},
		{sweepstate.ItemRefused, false, "r", "", true},
		{sweepstate.ItemRefused, false, "", "", false},
		{sweepstate.ItemFailed, false, "", "", true},
		{sweepstate.ItemFailed, true, "", "", true},
		{sweepstate.ItemFailed, false, "r", "", true},
		{sweepstate.NoPack, false, "", "", true},
		{sweepstate.NoPack, true, "", "", true},
		{sweepstate.NoPack, false, "r", "", false},
		{sweepstate.WouldGrab, true, "r", "", true},
		{sweepstate.WouldGrab, false, "r", "", false},
		{sweepstate.WouldGrab, true, "", "", false},
		{sweepstate.Searched, false, "", "e", true},
		{sweepstate.Searched, false, "", "", false},
		{sweepstate.Searched, true, "", "e", false},
		{sweepstate.Searched, false, "r", "e", false},
		{sweepstate.WouldSearch, true, "", "e", true},
		{sweepstate.WouldSearch, true, "", "", false},
		{sweepstate.WouldSearch, false, "", "e", false},
		{sweepstate.WouldSearch, true, "r", "e", false},
		{sweepstate.ItemNone, false, "", "", false},
		{"bogus", false, "", "", false},
	}
	for _, x := range cases {
		if err := sweepstate.CheckItem(x.r, x.dry, x.rel, x.eps); (err == nil) != x.ok {
			t.Errorf("CheckItem(%q, dry=%v, %q, %q) = %v, want ok = %v", x.r, x.dry, x.rel, x.eps, err, x.ok)
		}
	}
}

func TestMachineDiagram_Item(t *testing.T) {
	t.Parallel()
	machinetest.GoldenMermaid(t, itemMachine(), "testdata/item.md")
}
