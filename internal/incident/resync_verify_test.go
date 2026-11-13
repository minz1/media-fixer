package incident_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestS1_ResyncVerifyNotYet(t *testing.T) {
	t.Parallel()
	ag := newObservingAgent()
	ag.resolved.Store(true)
	ag.scan.Store(true)
	env := newLifecycleEnv(t, ag)
	s := env.verifying(t, "Resync wait", "subtitles")
	lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) {
		n.Verify.ChecksLeft = 1
		n.Verify.Spec.Kind = lifecycle.VerifyResync
		n.Verify.Spec.Resync = &lifecycle.ResyncTarget{ID: 66, Path: "/media/tv/Show/e6.en.srt", Language: "en"}
	})
	env.svc.StartRecovery()
	got := env.waitStatusWithin(t, s.ID, lifecycle.ManualTestNeeded, notifyWaitTimeout)
	if got.Verify != nil && got.Verify.Spec.Extensions != 0 {
		t.Fatalf("a scan extended the resync wait: %+v", got.Verify)
	}
	if ag.verifies.Load() != 0 || ag.scans.Load() != 0 {
		t.Fatalf("generic path ran: VerifyResolved %d, ScanRunning %d", ag.verifies.Load(), ag.scans.Load())
	}
}
