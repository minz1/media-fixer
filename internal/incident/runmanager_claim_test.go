package incident_test

import "testing"

func TestClaimCheck_StaleClaimKeepsNewerInflight(t *testing.T) {
	t.Parallel()
	svc := newLifecycleEnv(t, nil).svc
	doneNew := svc.ClaimCheckForTest("inc", 6)
	doneOld := svc.ClaimCheckForTest("inc", 5)
	if got := svc.ArmedForTest("inc"); got != 6 {
		t.Fatalf("armed gen after a stale claim = %d, want 6", got)
	}
	doneOld()
	if got := svc.ArmedForTest("inc"); got != 6 {
		t.Fatalf("armed gen after the stale check finished = %d, want 6", got)
	}
	doneNew()
	if got := svc.ArmedForTest("inc"); got != 0 {
		t.Fatalf("armed gen after both finished = %d, want none", got)
	}
}
