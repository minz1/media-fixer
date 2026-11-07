package lifecycletest_test

import (
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestSeed_ReachesEveryState(t *testing.T) {
	t.Parallel()
	d, err := db.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, st := range lifecycle.States() {
		s := lifecycletest.Seed(t, d, lifecycletest.Spec{Title: "Seed " + string(st), State: st})
		got, gerr := d.GetIncidentSnapshot(t.Context(), s.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if got.Status != st {
			t.Errorf("seeded %s, stored %s", st, got.Status)
		}
		if cerr := lifecycle.Check(got); cerr != nil {
			t.Errorf("%s: %v", st, cerr)
		}
	}
	mtn := lifecycletest.Seed(t, d, lifecycletest.Spec{
		Title: "Seed plan", State: lifecycle.ManualTestNeeded, Finding: lifecycletest.Escalatable(),
		Plan: []byte(`{"files":[]}`),
	})
	if mtn.Plan == nil || !mtn.Finding.Escalatable() {
		t.Fatalf("mtn with a plan: %+v", mtn)
	}
	nr := lifecycletest.Seed(t, d, lifecycletest.Spec{
		Title: "Seed no release", State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	if nr.Reason != lifecycle.ReasonNoRelease || nr.Download == nil {
		t.Fatalf("no_release seed: %+v", nr)
	}
}
