package lifecycle_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func TestD7_SwapEscalatable(t *testing.T) {
	t.Parallel()
	f := lifecycle.Finding{
		EscalateAction: lifecycle.EscalateSwapRelease, JSON: json.RawMessage(`{"escalate_action":"swap_release"}`),
	}
	if !f.Escalatable() {
		t.Fatal("a swap finding is not escalatable")
	}
	s := with(snap(lifecycle.ManualTestNeeded), func(s *lifecycle.Snapshot) { s.Finding = f })
	if got := kinds(s); !slices.Contains(got, lifecycle.ActPreview) || slices.Contains(got, lifecycle.ActApprove) {
		t.Fatalf("actions %v, want Preview and no Approve before a plan", got)
	}
	d := mustApply(t, s, tev{ev: lifecycle.Event{Kind: lifecycle.OwnerPreview, Plan: []byte(`{}`), PlanFP: "fp"}})
	if d.Next.Plan == nil {
		t.Fatal("preview stored no plan")
	}
}
