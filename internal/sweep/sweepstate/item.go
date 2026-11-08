package sweepstate

import (
	"errors"
	"fmt"
	"slices"
)

type ItemResult string

const (
	ItemNone    ItemResult = ""
	Grabbing    ItemResult = "grabbing"
	Imported    ItemResult = "imported"
	ItemFailed  ItemResult = "failed"
	ItemRefused ItemResult = "refused"
	Archive     ItemResult = "archive"
	Downloading ItemResult = "downloading"
	NoPack      ItemResult = "no_pack"
	WouldGrab   ItemResult = "would_grab"
	Searched    ItemResult = "searched"
	WouldSearch ItemResult = "would_search"
)

type ItemEventKind int

const (
	EvInserted ItemEventKind = iota
	EvMoved
)

type ItemEvent struct {
	Kind         ItemEventKind
	To           ItemResult
	Dry          bool
	ReleaseTitle string
	Episodes     string
}

type ItemSnap struct {
	Result       ItemResult
	Dry          bool
	ReleaseTitle string
	Episodes     string
}

type ItemRow struct {
	From   []ItemResult
	Kind   ItemEventKind
	To     []ItemResult
	Reject string
}

type ItemMeta struct {
	Initial      bool
	Grab         bool
	DryOnly      bool
	LiveOnly     bool
	NeedsRelease bool
	NoRelease    bool
}

func ItemResults() []ItemResult {
	return []ItemResult{
		Grabbing, Imported, ItemFailed, ItemRefused, Archive, Downloading, NoPack, WouldGrab, Searched, WouldSearch,
	}
}

func ItemMetaOf(r ItemResult) ItemMeta {
	switch r {
	case Grabbing:
		return ItemMeta{Initial: true, Grab: true, LiveOnly: true, NeedsRelease: true}
	case Imported, ItemRefused, Archive, Downloading:
		return ItemMeta{Grab: true, LiveOnly: true, NeedsRelease: true}
	case ItemFailed:
		return ItemMeta{Initial: true, Grab: true}
	case NoPack:
		return ItemMeta{Initial: true, NoRelease: true}
	case WouldGrab:
		return ItemMeta{Initial: true, DryOnly: true, NeedsRelease: true}
	case Searched:
		return ItemMeta{Initial: true, LiveOnly: true, NoRelease: true}
	case WouldSearch:
		return ItemMeta{Initial: true, DryOnly: true, NoRelease: true}
	case ItemNone:
		return ItemMeta{}
	}
	return ItemMeta{}
}

func ItemsWhere(pred func(ItemMeta) bool) []ItemResult {
	var out []ItemResult
	for _, r := range ItemResults() {
		if pred(ItemMetaOf(r)) {
			out = append(out, r)
		}
	}
	return out
}

func allTargets() []ItemResult {
	return append([]ItemResult{ItemNone}, ItemResults()...)
}

func except(all []ItemResult, drop ...ItemResult) []ItemResult {
	return slices.DeleteFunc(slices.Clone(all), func(r ItemResult) bool { return slices.Contains(drop, r) })
}

func itemMoves() []ItemRow {
	targets := allTargets()
	none := []ItemResult{ItemNone}
	stored := ItemResults()
	grabOutcomes := []ItemResult{Imported, ItemFailed, Downloading, Archive}
	return []ItemRow{
		{From: none, Kind: EvInserted, To: ItemsWhere(func(m ItemMeta) bool { return m.Initial })},
		{From: none, Kind: EvInserted, To: targets, Reject: "illegal sweep item insert"},
		{From: none, Kind: EvMoved, To: targets, Reject: "no row"},
		{From: stored, Kind: EvInserted, To: targets, Reject: "already inserted"},
		{From: []ItemResult{Grabbing}, Kind: EvMoved, To: grabOutcomes},
		{From: []ItemResult{Grabbing}, Kind: EvMoved, To: []ItemResult{ItemRefused}},
		{From: []ItemResult{Searched}, Kind: EvMoved, To: []ItemResult{ItemFailed}},
		{
			From: []ItemResult{Grabbing}, Kind: EvMoved, Reject: "illegal sweep item move",
			To: except(targets, Imported, ItemFailed, Downloading, Archive, ItemRefused),
		},
		{
			From: []ItemResult{Searched}, Kind: EvMoved, Reject: "illegal sweep item move",
			To: except(targets, ItemFailed),
		},
		{
			From: except(stored, Grabbing, Searched), Kind: EvMoved, To: targets,
			Reject: "sweep item already settled",
		},
	}
}

func insertable(ev ItemEvent) bool {
	return ItemMetaOf(ev.To).Initial && CheckItem(ev.To, ev.Dry, ev.ReleaseTitle, ev.Episodes) == nil
}

func itemRowMatches(r ItemRow, cur ItemSnap, ev ItemEvent) bool {
	if r.Kind != ev.Kind || !slices.Contains(r.From, cur.Result) || !slices.Contains(r.To, ev.To) {
		return false
	}
	if ev.Kind == EvInserted && cur.Result == ItemNone {
		return insertable(ev) == (r.Reject == "")
	}
	return true
}

func ItemRowsFor(cur ItemSnap, ev ItemEvent) int {
	n := 0
	for _, r := range itemMoves() {
		if itemRowMatches(r, cur, ev) {
			n++
		}
	}
	return n
}

func moveAllowed(r ItemRow) bool { return r.Reject == "" }

func NextItem(cur ItemSnap, ev ItemEvent) (ItemSnap, error) {
	var hit *ItemRow
	rows := itemMoves()
	for i := range rows {
		if !itemRowMatches(rows[i], cur, ev) {
			continue
		}
		if hit != nil {
			return cur, fmt.Errorf("sweep item %q: more than one transition matches %+v", cur.Result, ev)
		}
		hit = &rows[i]
	}
	if hit == nil {
		return cur, fmt.Errorf("sweep item %q: no transition for %+v", cur.Result, ev)
	}
	if !moveAllowed(*hit) {
		return cur, errors.New(hit.Reject)
	}
	if ev.Kind == EvInserted {
		return ItemSnap{Result: ev.To, Dry: ev.Dry, ReleaseTitle: ev.ReleaseTitle, Episodes: ev.Episodes}, nil
	}
	next := cur
	next.Result = ev.To
	return next, nil
}

func Move(from, to ItemResult) error {
	_, err := NextItem(ItemSnap{Result: from}, ItemEvent{Kind: EvMoved, To: to})
	return err
}

func CheckItem(r ItemResult, dry bool, releaseTitle, episodes string) error {
	m := ItemMetaOf(r)
	if !slices.Contains(ItemResults(), r) {
		return fmt.Errorf("%q is not a stored sweep item result", r)
	}
	switch {
	case m.DryOnly && !dry:
		return fmt.Errorf("%s item must be a dry item", r)
	case m.LiveOnly && dry:
		return fmt.Errorf("%s item must be a live item", r)
	case m.NeedsRelease && releaseTitle == "":
		return fmt.Errorf("%s item needs a release title", r)
	case m.NoRelease && releaseTitle != "":
		return fmt.Errorf("%s item has no release title", r)
	case (r == Searched || r == WouldSearch) && episodes == "":
		return fmt.Errorf("%s item needs episodes", r)
	}
	return nil
}
