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

func (e *lcEnv) waitFixed(t *testing.T, id string) {
	e.waitStatusWithin(t, id, lifecycle.AgentFixed, notifyWaitTimeout)
}

func (e *lcEnv) waitStatusWithin(
	t *testing.T, id string, want lifecycle.State, timeout time.Duration,
) lifecycle.Snapshot {
	t.Helper()
	var got lifecycle.Snapshot
	eventuallyWithin(t, timeout, "status "+string(want), func() bool {
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
	env.waitFixed(t, s.ID)
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
	got := env.waitStatusWithin(t, inc.ID, lifecycle.Verifying, loadedWaitTimeout)
	eventuallyWithin(t, loadedWaitTimeout, "the armed check", func() bool {
		return env.svc.ArmedForTest(inc.ID) == got.Generation
	})
	stopService(t, env.svc)
	next := incident.NewService(t.Context(), env.db, nil, ag, nil, nil, env.notif, env.svc.LogForTest())
	t.Cleanup(func() { stopService(t, next) })
	next.StartRecovery()
	eventuallyWithin(
		t,
		loadedWaitTimeout,
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
	env.waitFixed(t, s.ID)
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
	env.waitFixed(t, s.ID)
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

type cancelingObserver struct {
	*observingAgent

	cancel context.CancelFunc
}

func (a *cancelingObserver) VerifyResolved(ctx context.Context, itemID, title string, pre *agent.FixSignature) bool {
	a.cancel()
	return a.observingAgent.VerifyResolved(ctx, itemID, title, pre)
}

func TestChecks_CanceledApplyLogsInfo(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	ag := &cancelingObserver{observingAgent: newObservingAgent(), cancel: cancel}
	env := newLifecycleEnv(t, ag)
	s := env.verifying(t, "Canceled check", "cant_play")
	env.svc.CheckNowForTest(ctx, s.ID)
	if ag.verifies.Load() != 1 {
		t.Fatalf("VerifyResolved calls %d, want 1", ag.verifies.Load())
	}
	if env.logged("level=ERROR") || !env.logged("check result dropped, the check was stopped") {
		t.Fatalf("canceled check logs:\n%s", env.logs.String())
	}
}

func TestQ4b_4_StuckReadVerifyUnknownExtends(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	chk := &fakeStuckChecker{state: func(int) incident.ReadState { return incident.ReadUnknown }}
	env.svc.SetStuckRead(chk)
	s := env.verifying(t, "Stuck unknown", lifecycle.WhatStuckRead)
	env.svc.StartRecovery()
	var got lifecycle.Snapshot
	eventually(t, "the unknown extension", func() bool {
		got = env.snap(t, s.ID)
		return got.Verify != nil && got.Verify.Spec.Extensions == 1
	})
	if got.Status != lifecycle.Verifying || got.Verify.ChecksLeft != s.Verify.ChecksLeft {
		t.Fatalf("unknown read state changed %+v", got)
	}
}

func TestE2_ClearBeforeFloorArmsAtFloor(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	chk := &fakeStuckChecker{clear: func(int) bool { return true }}
	env.svc.SetStuckRead(chk)
	s := env.verifying(t, "Floor show", lifecycle.WhatStuckRead)
	entered := time.Now()
	lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) { n.Verify.Spec.VerifyEnteredAt = entered })

	if err := env.svc.ArmStuckReadClear(t.Context(), "Floor show"); err != nil {
		t.Fatal(err)
	}
	at, ok := env.svc.ArmedAtForTest(s.ID)
	if !ok {
		t.Fatal("the clear did not arm the verify check")
	}
	if d := at.Sub(entered); d < 3*time.Minute || d > 3*time.Minute+2*time.Second {
		t.Fatalf("armed %v after the entered time, want the 3-minute floor", d)
	}
}

func TestE2_ClearAfterFloorConfirmsQuickly(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	chk := &fakeStuckChecker{clear: func(int) bool { return true }}
	env.svc.SetStuckRead(chk)
	s := env.verifying(t, "Quick show", lifecycle.WhatStuckRead)
	lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) {
		n.Verify.Spec.VerifyEnteredAt = time.Now().Add(-4 * time.Minute)
	})

	if err := env.svc.ArmStuckReadClear(t.Context(), "Quick show"); err != nil {
		t.Fatal(err)
	}
	env.waitFixed(t, s.ID)
	if n := chk.calls(); n != 1 {
		t.Fatalf("Clear calls = %d, want 1", n)
	}
}

func TestE2_OtherTitlesAndKindsUntouched(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	chk := &fakeStuckChecker{clear: func(int) bool { return true }}
	env.svc.SetStuckRead(chk)
	target := env.verifying(t, "Target show", lifecycle.WhatStuckRead)
	otherTitle := env.verifying(t, "Other show", lifecycle.WhatStuckRead)
	generic := env.verifying(t, "Generic show", "cant_play")
	sameTitleGeneric := env.verifying(t, "Same title", lifecycle.WhatStuckRead)
	lifecycletest.Force(t, env.db, sameTitleGeneric.ID, func(n *lifecycle.Snapshot) {
		n.Verify.Spec.Kind = lifecycle.VerifyGeneric
	})

	if err := env.svc.ArmStuckReadClear(t.Context(), "Target show"); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.svc.ArmedAtForTest(target.ID); !ok {
		t.Fatal("the matching stuck-read verify was not armed")
	}
	if _, ok := env.svc.ArmedAtForTest(otherTitle.ID); ok {
		t.Fatal("a stuck-read verify with a different title was armed")
	}
	if _, ok := env.svc.ArmedAtForTest(generic.ID); ok {
		t.Fatal("a generic verify was armed")
	}
	if err := env.svc.ArmStuckReadClear(t.Context(), "Same title"); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.svc.ArmedAtForTest(sameTitleGeneric.ID); ok {
		t.Fatal("a same-title generic verify was armed")
	}
}

func TestE2_ClearArmsEarlierThanTheVerifyTimer(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	env.svc.SetStuckRead(&fakeStuckChecker{clear: func(int) bool { return true }})
	s := env.verifying(t, "Timer show", lifecycle.WhatStuckRead)
	s = lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) {
		n.Verify.Spec.VerifyEnteredAt = time.Now().Add(-10 * time.Minute)
	})
	inc := &db.Incident{ID: s.ID}
	env.svc.ScheduleCheckForTest(inc, s.Generation, time.Now().Add(time.Hour))

	if err := env.svc.ArmStuckReadClear(t.Context(), "Timer show"); err != nil {
		t.Fatal(err)
	}
	at, ok := env.svc.ArmedAtForTest(s.ID)
	if ok && at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("the clear left the verify check armed at %v, want now", at)
	}
	env.waitFixed(t, s.ID)
}

func TestF35_RefusedEarlyWakeIsLogged(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	env.svc.SetStuckRead(&fakeStuckChecker{clear: func(int) bool { return true }})
	s := env.verifying(t, "Refused show", lifecycle.WhatStuckRead)
	lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) {
		n.Verify.Spec.VerifyEnteredAt = time.Now()
	})
	if err := env.svc.ArmStuckReadClear(t.Context(), "Refused show"); err != nil {
		t.Fatal(err)
	}
	soon, ok := env.svc.ArmedAtForTest(s.ID)
	if !ok {
		t.Fatal("the first clear did not arm the verify check")
	}
	if env.logged("early check wake refused") {
		t.Fatal("an accepted early wake was logged as refused")
	}
	lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) {
		n.Verify.Spec.VerifyEnteredAt = time.Now().Add(time.Hour)
	})

	if err := env.svc.ArmStuckReadClear(t.Context(), "Refused show"); err != nil {
		t.Fatal(err)
	}
	if at, armed := env.svc.ArmedAtForTest(s.ID); !armed || !at.Equal(soon) {
		t.Fatalf("armed at %v (%v), want the sooner timer kept at %v", at, armed, soon)
	}
	if !env.logged("early check wake refused") {
		t.Fatalf("the refused early wake was not logged:\n%s", env.logs.String())
	}
}

func TestE2_NoEnteredTimeLeavesTheVerifyTimer(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	env.svc.SetStuckRead(&fakeStuckChecker{clear: func(int) bool { return true }})
	s := env.verifying(t, "Old row", lifecycle.WhatStuckRead)
	s = lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) {
		n.Verify.Spec.VerifyEnteredAt = time.Time{}
	})

	if err := env.svc.ArmStuckReadClear(t.Context(), "Old row"); err != nil {
		t.Fatal(err)
	}
	if at, ok := env.svc.ArmedAtForTest(s.ID); ok {
		t.Fatalf("a row with no verify entry time was armed at %v, want the timer left alone", at)
	}
}

func (a *observingAgent) VerifySubtitle(context.Context, string, string) bool { return true }

func (a *blockingObserver) VerifySubtitle(context.Context, string, string) bool { return true }

func (a *cancelingObserver) VerifySubtitle(context.Context, string, string) bool { return true }

type subtitleAgent struct {
	*observingAgent

	result agent.DiagnosticResult
	subs   atomic.Int32
	item   atomic.Value
	lang   atomic.Value
}

func (a *subtitleAgent) Run(
	ctx context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	r := a.result
	return gatedExit(ctx, gate, &r, nil, nil)
}

func (a *subtitleAgent) VerifySubtitle(_ context.Context, itemID, lang string) bool {
	a.subs.Add(1)
	a.item.Store(itemID)
	a.lang.Store(lang)
	return true
}

func subtitleResult(subtitleItem string) agent.DiagnosticResult {
	return agent.DiagnosticResult{
		RootCause: "test", Confidence: "high", PrimaryAction: "run-a", PrimaryReason: "test",
		VerifyAfterSeconds: 3600, VerifyItemID: "ep1",
		VerifySubtitleLanguage: "en", SubtitleItemID: subtitleItem,
	}
}

func TestB6_RunVerificationUsesSubtitleKind(t *testing.T) {
	t.Parallel()
	report := func(what string) *incident.Report {
		return &incident.Report{Source: "discord", ReportedBy: "alice", What: what, Title: "Show"}
	}
	ag := &subtitleAgent{observingAgent: newObservingAgent(), result: subtitleResult("ep1")}
	env := newLifecycleEnv(t, ag)
	inc, err := env.svc.Handle(t.Context(), report(agent.WhatSubtitles))
	if err != nil {
		t.Fatal(err)
	}
	got := env.waitStatusWithin(t, inc.ID, lifecycle.Verifying, loadedWaitTimeout)
	if sp := got.Verify.Spec; sp.Kind != lifecycle.VerifySubtitles || sp.Language != "eng" ||
		sp.SubtitleItemID != "ep1" || sp.ItemID != "ep1" {
		t.Fatalf("spec = %+v", sp)
	}
	env = newLifecycleEnv(t, ag)
	s := env.verifying(t, "Show", agent.WhatSubtitles)
	lifecycletest.Force(t, env.db, s.ID, func(n *lifecycle.Snapshot) { n.Verify.Spec = got.Verify.Spec })
	env.svc.StartRecovery()
	env.waitFixed(t, s.ID)
	if ag.subs.Load() != 1 || ag.verifies.Load() != 0 || ag.item.Load() != "ep1" || ag.lang.Load() != "eng" {
		t.Fatalf("VerifySubtitle %d (%v %v), VerifyResolved %d", ag.subs.Load(), ag.item.Load(), ag.lang.Load(),
			ag.verifies.Load())
	}

	empty := &subtitleAgent{observingAgent: newObservingAgent(), result: subtitleResult("")}
	env = newLifecycleEnv(t, empty)
	inc, err = env.svc.Handle(t.Context(), report(agent.WhatSubtitles))
	if err != nil {
		t.Fatal(err)
	}
	got = env.waitStatusWithin(t, inc.ID, lifecycle.ManualTestNeeded, loadedWaitTimeout)
	if got.Verify != nil && got.Verify.Spec.Kind == lifecycle.VerifySubtitles {
		t.Fatalf("a subtitle verify was armed with no item: %+v", got.Verify)
	}

	generic := &subtitleAgent{observingAgent: newObservingAgent(), result: subtitleResult("ep1")}
	env = newLifecycleEnv(t, generic)
	inc, err = env.svc.Handle(t.Context(), report("cant_play"))
	if err != nil {
		t.Fatal(err)
	}
	got = env.waitStatusWithin(t, inc.ID, lifecycle.Verifying, loadedWaitTimeout)
	if got.Verify.Spec.Kind != lifecycle.VerifyGeneric {
		t.Fatalf("cant_play kind = %q", got.Verify.Spec.Kind)
	}
}
