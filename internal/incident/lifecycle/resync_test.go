package lifecycle_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func resyncFinding() lifecycle.Finding {
	return lifecycle.Finding{
		EscalateAction: lifecycle.EscalateResyncSubtitle,
		JSON:           json.RawMessage(`{"escalate_action":"resync_subtitle"}`),
	}
}

func TestS1_ResyncApproveRefusedOnD(t *testing.T) {
	t.Parallel()
	if !resyncFinding().Escalatable() {
		t.Fatal("a resync finding is not escalatable")
	}
	from := freshMtn(finding(resyncFinding()))
	d := decide(t, from, buildErrEv())
	n := d.Next
	if n.Status != lifecycle.ManualTestNeeded || n.Reason != lifecycle.ReasonPlanFailed || n.Op != nil ||
		n.Plan != nil {
		t.Fatalf("next %s/%s op %v plan %v", n.Status, n.Reason, n.Op, n.Plan)
	}
	if d.OpWrite != nil || d.OpNew {
		t.Errorf("op write %+v new=%v", d.OpWrite, d.OpNew)
	}
	if got := effectKinds(d.Effects); !slices.Equal(got, []K{ownerK}) {
		t.Errorf("effects %v", got)
	}
	if got := ownerNotice(t, d).Err; got != "premise changed" {
		t.Errorf("notice error %q", got)
	}
}
