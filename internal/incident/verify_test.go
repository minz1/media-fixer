package incident_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

type observingAgent struct {
	*lowConfidenceOnceAgent

	resolved atomic.Bool
	scan     atomic.Bool
	verifies atomic.Int32
	scans    atomic.Int32
	verifyAt int
}

func newObservingAgent() *observingAgent {
	return &observingAgent{lowConfidenceOnceAgent: newLowConfidenceOnceAgent()}
}

func (a *observingAgent) Run(
	ctx context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	return gatedExit(ctx, gate, &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high", PrimaryAction: "run-a", PrimaryReason: "test",
		VerifyAfterSeconds: a.verifyAt,
	}, nil, nil)
}

func (a *observingAgent) VerifyResolved(context.Context, string, string, *agent.FixSignature) bool {
	a.verifies.Add(1)
	return a.resolved.Load()
}

func (a *observingAgent) ScanRunning(context.Context) bool {
	a.scans.Add(1)
	return a.scan.Load()
}

func (e *lcEnv) verifying(t *testing.T, title, what string) lifecycle.Snapshot {
	t.Helper()
	s := lifecycletest.Seed(t, e.db, lifecycletest.Spec{Title: title, What: what, State: lifecycle.Verifying})
	lifecycletest.Force(t, e.db, s.ID, func(n *lifecycle.Snapshot) { n.NextCheck = time.Now().Add(-time.Minute) })
	return s
}

func (e *lcEnv) waitStatus(t *testing.T, id string, want lifecycle.State) lifecycle.Snapshot {
	t.Helper()
	var got lifecycle.Snapshot
	eventually(t, "status "+string(want), func() bool {
		got = e.snap(t, id)
		return got.Status == want
	})
	return got
}

func TestI1_11_VerifyingSurvivesRestart(t *testing.T) {
	t.Parallel()
	ag := newObservingAgent()
	ag.resolved.Store(true)
	env := newLifecycleEnv(t, ag)
	s := env.verifying(t, "Survives restart", "cant_play")
	env.svc.StartRecovery()
	env.waitStatus(t, s.ID, lifecycle.AgentFixed)
	eventually(t, "the fixed note", func() bool {
		cards := env.notif.userCards()
		return len(cards) == 1 && strings.Contains(cards[0].Plain(), "fixed automatically")
	})
}

func TestI1_12_ServiceScanRunningRechecks(t *testing.T) {
	t.Parallel()
	ag := newObservingAgent()
	ag.scan.Store(true)
	env := newLifecycleEnv(t, ag)
	s := env.verifying(t, "Scan running", "cant_play")
	lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) { n.Verify.ChecksLeft = 1 })
	env.svc.StartRecovery()
	var got lifecycle.Snapshot
	eventually(t, "the extension", func() bool {
		got = env.snap(t, s.ID)
		return got.Verify != nil && got.Verify.Spec.Extensions == 1
	})
	if got.Status != lifecycle.Verifying || !got.Verify.Spec.ETANoted || ag.scans.Load() != 1 {
		t.Fatalf("after a scan-running last check: %+v scans=%d", got, ag.scans.Load())
	}
	eventually(t, "the recheck", func() bool { return env.svc.ArmedForTest(s.ID) == got.Generation })
	eventually(t, "the ETA note", func() bool {
		cards := env.notif.userCards()
		return len(cards) == 1 && strings.Contains(cards[0].Plain(), "still rebuilding")
	})
}

func TestI8_4_VerifyingAlwaysHasDriver(t *testing.T) {
	t.Parallel()
	ag := newObservingAgent()
	ag.verifyAt = 3600
	env := newLifecycleEnv(t, ag)
	inc, err := env.svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "Has driver",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := env.waitStatus(t, inc.ID, lifecycle.Verifying)
	eventually(t, "the armed check", func() bool { return env.svc.ArmedForTest(inc.ID) == got.Generation })
	stopService(t, env.svc)
	next := incident.NewService(t.Context(), env.db, nil, ag, nil, nil, env.notif, env.svc.LogForTest())
	t.Cleanup(func() { stopService(t, next) })
	next.StartRecovery()
	eventually(
		t,
		"the re-armed check after a restart",
		func() bool { return next.ArmedForTest(inc.ID) == got.Generation },
	)
}

func TestVerify_UnknownKeepsVerifying(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	s := env.verifying(t, "Unknown result", lifecycle.WhatStuckRead)
	env.svc.StartRecovery()
	var got lifecycle.Snapshot
	eventually(t, "the unknown extension", func() bool {
		got = env.snap(t, s.ID)
		return got.Verify != nil && got.Verify.Spec.Extensions == 1
	})
	if got.Status != lifecycle.Verifying || got.Verify.ChecksLeft != s.Verify.ChecksLeft {
		t.Fatalf("unknown result changed %+v", got)
	}
}

func TestVerify_StuckReadUsesClear(t *testing.T) {
	t.Parallel()
	ag := newObservingAgent()
	env := newLifecycleEnv(t, ag)
	chk := &fakeStuckChecker{clear: func(int) bool { return true }}
	env.svc.SetStuckRead(chk)
	s := env.verifying(t, "Stuck clear", lifecycle.WhatStuckRead)
	env.svc.StartRecovery()
	env.waitStatus(t, s.ID, lifecycle.AgentFixed)
	if chk.calls() != 1 || ag.verifies.Load() != 0 {
		t.Fatalf("Clear calls %d, VerifyResolved calls %d", chk.calls(), ag.verifies.Load())
	}
}

func TestChecks_ReadOnlyWhilePaused(t *testing.T) {
	t.Parallel()
	ag := newObservingAgent()
	ag.resolved.Store(true)
	env := newLifecycleEnv(t, ag)
	s := env.verifying(t, "Paused check", "cant_play")
	if err := env.svc.SetAutonomousPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	env.svc.StartRecovery()
	env.waitStatus(t, s.ID, lifecycle.AgentFixed)
}

type blockingObserver struct {
	*observingAgent

	entered chan struct{}
	release chan struct{}
}

func (a *blockingObserver) VerifyResolved(ctx context.Context, itemID, title string, pre *agent.FixSignature) bool {
	a.entered <- struct{}{}
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	return a.observingAgent.VerifyResolved(ctx, itemID, title, pre)
}

func TestReconciler_RunningCheckCountsAsArmed(t *testing.T) {
	t.Parallel()
	ag := &blockingObserver{observingAgent: newObservingAgent(), entered: make(chan struct{}, 4),
		release: make(chan struct{})}
	env := newLifecycleEnv(t, ag)
	s := env.verifying(t, "Running check", "cant_play")
	env.svc.StartRecovery()
	select {
	case <-ag.entered:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the first check")
	}
	if c := env.svc.ReconcileForTest(t.Context()); c.Checks != 0 {
		t.Fatalf("reconcile armed %d checks while one was running", c.Checks)
	}
	if got := env.svc.ArmedForTest(s.ID); got != s.Generation {
		t.Fatalf("running check reported armed at gen %d, want %d", got, s.Generation)
	}
	close(ag.release)
	var got lifecycle.Snapshot
	eventually(t, "the check result", func() bool {
		got = env.snap(t, s.ID)
		return got.Verify != nil && got.Verify.ChecksLeft == s.Verify.ChecksLeft-1
	})
	eventually(t, "the recheck", func() bool { return env.svc.ArmedForTest(s.ID) == got.Generation })
	if n := ag.verifies.Load(); n != 1 {
		t.Fatalf("VerifyResolved calls %d, want 1", n)
	}
}
