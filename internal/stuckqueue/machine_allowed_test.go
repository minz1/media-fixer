package stuckqueue_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func smResultEvents(s stuckqueue.Snapshot, a stuckqueue.OwnerAction, at time.Time) []stuckqueue.Event {
	files := stuckqueue.Files{N: 2, M: 2}
	none := stuckqueue.Files{}
	switch a {
	case stuckqueue.OwnerRemove:
		return []stuckqueue.Event{smActedEv(at, stuckqueue.ActionRemove, 0, false, none)}
	case stuckqueue.OwnerRegrab:
		return []stuckqueue.Event{smActedEv(at, stuckqueue.ActionRegrab, 3, false, none)}
	case stuckqueue.OwnerImport:
		return []stuckqueue.Event{
			smPosted(at, 7, false, files),
			smActedEv(at, stuckqueue.ActionImport, 0, false, files),
		}
	case stuckqueue.OwnerMappedImport:
		return []stuckqueue.Event{smPosted(at, 7, true, files)}
	case stuckqueue.OwnerResume:
		op := int64(1)
		if s.Acted != nil && s.Acted.OpID > 0 {
			op = s.Acted.OpID
		}
		return []stuckqueue.Event{
			smOpEv(stuckqueue.EvResumed, at, op, true),
			smOpEv(stuckqueue.EvResumed, at, op, false),
		}
	}
	return nil
}

func TestAllowed_MatchesTable(t *testing.T) {
	t.Parallel()
	nows := []time.Time{smAt(0), smAt(30 * time.Minute), smAt(2 * time.Hour), smAt(17 * 24 * time.Hour)}
	for _, s := range smAll() {
		for _, now := range nows {
			for _, a := range stuckqueue.OwnerActions() {
				allowed := stuckqueue.Allowed(s, a, now, smMinAge) == nil
				for _, e := range smResultEvents(s, a, now) {
					_, _, err := stuckqueue.Next(s, e)
					if (err == nil) != allowed {
						t.Errorf("%s now %s %s: Allowed = %v, Next(%s) = %v", smKeyOf(s), now, a, allowed, e.Kind, err)
					}
				}
			}
		}
	}
}

func TestAllowed_Texts(t *testing.T) {
	t.Parallel()
	const changed = "this changed, look again"
	cases := []struct {
		name string
		s    stuckqueue.Snapshot
		a    stuckqueue.OwnerAction
		now  time.Time
		want string
	}{
		{"untracked regrab", smUntracked(), stuckqueue.OwnerRegrab, smAt(0),
			"no run has checked this download yet; only Remove is allowed until one does"},
		{"untracked mapped", smUntracked(), stuckqueue.OwnerMappedImport, smAt(0),
			"no run has checked this download yet; only Remove is allowed until one does"},
		{"untracked resume", smUntracked(), stuckqueue.OwnerResume, smAt(0), changed},
		{"in wait import", smWaiting(), stuckqueue.OwnerImport, smAt(30 * time.Minute),
			"this download is inside the 1 h wait until 2026-11-07 13:00 UTC; only Remove is allowed"},
		{"in wait resume", smWaiting(), stuckqueue.OwnerResume, smAt(0), changed},
		{"open resume", smNotified(), stuckqueue.OwnerResume, smAt(2 * time.Hour), changed},
		{"inflight remove", smInflight(), stuckqueue.OwnerRemove, smAt(0), "an import for this download is running"},
		{
			"inflight mapped",
			smInflight(),
			stuckqueue.OwnerMappedImport,
			smAt(0),
			"an import for this download is running",
		},
		{"inflight resume", smInflight(), stuckqueue.OwnerResume, smAt(0), changed},
		{"unfinished remove", smActedRegrab(stuckqueue.ResultUnfinished, 1), stuckqueue.OwnerRemove, smAt(0),
			"already re-grabbed at 2026-11-07 12:00 UTC"},
		{"unfinished mapped", smActedRegrab(stuckqueue.ResultUnfinished, 1), stuckqueue.OwnerMappedImport, smAt(0),
			"already re-grabbed at 2026-11-07 12:00 UTC"},
		{
			"removed regrab",
			smActedRemove(),
			stuckqueue.OwnerRegrab,
			smAt(time.Hour),
			"already removed at 2026-11-07 12:00 UTC",
		},
		{
			"imported import",
			smActedImport(2, 2),
			stuckqueue.OwnerImport,
			smAt(time.Hour),
			"already imported at 2026-11-07 12:00 UTC",
		},
		{"regrab ok resume", smActedRegrab(stuckqueue.ResultOK, 1), stuckqueue.OwnerResume, smAt(0), changed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := stuckqueue.Allowed(c.s, c.a, c.now, smMinAge)
			if !errors.Is(err, stuckqueue.ErrRefused) || !strings.HasSuffix(err.Error(), c.want) {
				t.Errorf("err = %v, want refusal %q", err, c.want)
			}
		})
	}
	ok := []struct {
		s stuckqueue.Snapshot
		a stuckqueue.OwnerAction
		n time.Time
	}{
		{smUntracked(), stuckqueue.OwnerRemove, smAt(0)},
		{smWaiting(), stuckqueue.OwnerRemove, smAt(0)},
		{smWaiting(), stuckqueue.OwnerImport, smAt(2 * time.Hour)},
		{smActedImport(1, 2), stuckqueue.OwnerMappedImport, smAt(time.Hour)},
		{smActedRegrab(stuckqueue.ResultUnfinished, 1), stuckqueue.OwnerResume, smAt(time.Hour)},
		{smNotified(), stuckqueue.OwnerRegrab, smAt(time.Hour)},
	}
	for _, c := range ok {
		if err := stuckqueue.Allowed(c.s, c.a, c.n, smMinAge); err != nil {
			t.Errorf("%s %s: %v", smKeyOf(c.s), c.a, err)
		}
	}
}

func TestActions_PerState(t *testing.T) {
	t.Parallel()
	now := smAt(2 * time.Hour)
	remove := []stuckqueue.Action{stuckqueue.ActionRemove}
	cases := []struct {
		name string
		s    stuckqueue.Snapshot
		now  time.Time
		want []stuckqueue.Action
	}{
		{"untracked", smUntracked(), now, remove},
		{"in wait", smWaiting(), smAt(0), remove},
		{"waiting past the wait", smWaiting(), now, remove},
		{"due", smDue(), now, stuckqueue.OfferAll.Actions()},
		{"held", smHeld(), now, remove},
		{"notified", smNotified(), now, stuckqueue.OfferAll.Actions()},
		{"cleared", smCleared(), now, remove},
		{"inflight", smInflight(), now, nil},
		{"remove", smActedRemove(), now, nil},
		{"import", smActedImport(2, 2), now, nil},
		{"regrab ok", smActedRegrab(stuckqueue.ResultOK, 1), now, nil},
		{"unfinished", smActedRegrab(stuckqueue.ResultUnfinished, 1), now, []stuckqueue.Action{"resume"}},
	}
	for _, c := range cases {
		if got := stuckqueue.Actions(c.s, c.now, smMinAge); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if got := stuckqueue.OfferAll.Actions(); !slices.Equal(got, []stuckqueue.Action{"import", "regrab", "remove"}) {
		t.Errorf("OfferAll = %v", got)
	}
	if got := stuckqueue.OfferNone.Actions(); len(got) != 0 {
		t.Errorf("OfferNone = %v", got)
	}
}

func TestStuckMeta_EveryState(t *testing.T) {
	t.Parallel()
	for _, st := range stuckqueue.States() {
		m := stuckqueue.Meta(stuckqueue.Snapshot{State: st})
		if m.Label == "" && st != stuckqueue.StateUntracked && st != stuckqueue.StateActed {
			t.Errorf("%s: no label", st)
		}
		if m.Stored != (st != stuckqueue.StateUntracked) || m.Terminal != (st == stuckqueue.StateUntracked) {
			t.Errorf("%s: stored %v terminal %v", st, m.Stored, m.Terminal)
		}
		if m.Tone == "" || len(m.Drivers) == 0 {
			t.Errorf("%s: tone %q drivers %v", st, m.Tone, m.Drivers)
		}
	}
	labels := map[string]stuckqueue.Snapshot{
		"Waiting": smWaiting(), "New": smDue(), "Held": smHeld(), "Stuck": smNotified(), "Importing": smInflight(),
		"Cleared": smCleared(), "Imported": smActedImport(2, 2), "Re-grabbed": smActedRegrab(stuckqueue.ResultOK, 1),
		"Removed": smActedRemove(), "Unfinished": smActedRegrab(stuckqueue.ResultUnfinished, 1),
	}
	for want, s := range labels {
		if got := stuckqueue.Meta(s); got.Label != want {
			t.Errorf("%s: label %q, want %q", smKeyOf(s), got.Label, want)
		}
	}
	if got := stuckqueue.Meta(labels["Unfinished"]); got.Tone != stuckqueue.ToneWarn ||
		!slices.Contains(got.Drivers, stuckqueue.DriverOwner) {
		t.Errorf("unfinished meta = %+v", got)
	}
	if got := stuckqueue.Meta(
		smInflight(),
	).Drivers; !slices.Equal(
		got,
		[]stuckqueue.Driver{stuckqueue.DriverHousekeeping},
	) {
		t.Errorf("inflight drivers = %v", got)
	}
	if got := stuckqueue.Meta(
		smActedRemove(),
	).Drivers; !slices.Equal(
		got,
		[]stuckqueue.Driver{stuckqueue.DriverRun, stuckqueue.DriverHousekeeping},
	) {
		t.Errorf("acted drivers = %v", got)
	}
	if got := stuckqueue.Meta(smHeld()).Drivers; !slices.Equal(got, []stuckqueue.Driver{stuckqueue.DriverRun}) {
		t.Errorf("held drivers = %v", got)
	}
}
