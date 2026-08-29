package incident_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	_ "modernc.org/sqlite" // register SQLite driver for the raw-backdate test helper

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
)

type captureNotifier struct{ msgs []string }

func (c *captureNotifier) NotifyOwner(_ context.Context, msg string) error {
	c.msgs = append(c.msgs, msg)
	return nil
}

func (c *captureNotifier) NotifyUser(_ context.Context, _, msg string) error {
	c.msgs = append(c.msgs, msg)
	return nil
}

func newTestService(t *testing.T) (*incident.Service, *db.DB, *captureNotifier) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	notif := &captureNotifier{}
	// agent is nil — tests must not trigger the agent goroutine, so all
	// incidents are created with a nil agent and the goroutine exits early.
	svc := incident.NewService(context.Background(), database, nil, nil, nil, nil, notif, slog.New(slog.DiscardHandler))
	return svc, database, notif
}

func TestHandle_CreatesIncident(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	rep := &incident.Report{
		Source: "seerr", ReportedBy: "alice",
		What: "cant_play", Title: "Breaking Bad",
	}
	inc, err := svc.Handle(ctx, rep)
	if err != nil {
		t.Fatal(err)
	}
	if inc.ID == "" {
		t.Fatal("expected ID")
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "seerr" {
		t.Errorf("source: %q", got.Source)
	}
	if got.Status != db.StatusOpen {
		t.Errorf("status: %q", got.Status)
	}
}

func TestHandle_DeduplicatesByTitle(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	rep := &incident.Report{Source: "seerr", ReportedBy: "alice", What: "cant_play", Title: "Sopranos"}

	inc1, err := svc.Handle(ctx, rep)
	if err != nil {
		t.Fatal(err)
	}

	rep.ReportedBy = "bob"
	inc2, err := svc.Handle(ctx, rep)
	if err != nil {
		t.Fatal(err)
	}

	if inc1.ID != inc2.ID {
		t.Errorf("expected dedup: got %q and %q", inc1.ID, inc2.ID)
	}

	reporters, err := database.ListReporters(ctx, inc1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) != 2 {
		t.Errorf("reporters: got %d want 2", len(reporters))
	}
}

const systemicThresholdTitles = 5

func TestHandle_SystemicLock(t *testing.T) {
	t.Parallel()
	svc, database, notif := newTestService(t)
	ctx := context.Background()

	// Create 5 open incidents to hit the threshold.
	titles := []string{"A", "B", "C", "D", "E"}
	if len(titles) != systemicThresholdTitles {
		t.Fatalf("test setup: expected %d titles", systemicThresholdTitles)
	}
	for _, title := range titles {
		if _, err := svc.Handle(ctx, &incident.Report{
			Source: "seerr", ReportedBy: "x",
			What: "cant_play", Title: title,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The 6th should be locked.
	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "seerr", ReportedBy: "x",
		What: "cant_play", Title: "F",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutonomousLocked {
		t.Error("expected autonomous_locked for 6th incident")
	}
	if got.Status != db.StatusBlocked {
		t.Errorf("expected status blocked for systemic-locked incident, got %q", got.Status)
	}
	if len(notif.msgs) == 0 {
		t.Error("expected owner notification for systemic lock")
	}
}

func TestUnlock(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "seerr", ReportedBy: "x", What: "cant_play", Title: "Locked Show",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lockErr := database.SetAutonomousLocked(ctx, inc.ID, true); lockErr != nil {
		t.Fatal(lockErr)
	}

	if unlockErr := svc.Unlock(ctx, inc.ID); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutonomousLocked {
		t.Error("expected incident to be unlocked")
	}
}

func TestRerunClearsLock(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "seerr", ReportedBy: "x", What: "cant_play", Title: "Blocked Show",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lockErr := database.SetAutonomousLocked(ctx, inc.ID, true); lockErr != nil {
		t.Fatal(lockErr)
	}

	// Rerun is a deliberate human override — it must clear the lock.
	if rerunErr := svc.Rerun(ctx, inc.ID); rerunErr != nil {
		t.Fatal(rerunErr)
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutonomousLocked {
		t.Error("rerun should clear the autonomous lock")
	}
	if got.Status != db.StatusReopened {
		t.Errorf("status after rerun: %q", got.Status)
	}
}

func TestResolveAndRerun(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice",
		What: "cant_play", Title: "Deadwood",
	})
	if err != nil {
		t.Fatal(err)
	}

	if resolveErr := svc.Resolve(ctx, inc.ID); resolveErr != nil {
		t.Fatal(resolveErr)
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusResolved {
		t.Errorf("status after resolve: %q", got.Status)
	}

	// Rerun with nil agent just marks it reopened (agent goroutine exits immediately).
	if rerunErr := svc.Rerun(ctx, inc.ID); rerunErr != nil {
		t.Fatal(rerunErr)
	}
	got, err = database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusReopened {
		t.Errorf("status after rerun: %q", got.Status)
	}
}

// TestStatusTransitions_NotifyLiveSubscribers is the regression test for a
// gap found by manually driving the live dashboard end to end (not by any
// unit test): Resolve/Rerun/Unlock/escalateToOwner/markFixedAndNotify all
// mutate status one layer above Agent.Run, which is the only place that used
// to notify the event log's subscribers (via recordRunFinished) — so a
// status change made through any of these methods was durably written but
// never made an open SSE connection re-render, and an already-terminal
// incident's stream never closed. Confirmed live: resolving an incident
// while its dashboard page was open left the page showing "investigating"
// until manually reloaded. Each of Service's own status-mutating methods
// must append a status_changed event (see recordStatusChanged) so
// journal.Subscribe's channel actually fires.
func TestStatusTransitions_NotifyLiveSubscribers(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	jrnl := journal.New(database)
	notif := &captureNotifier{}
	discard := slog.New(slog.DiscardHandler)
	svc := incident.NewService(context.Background(), database, jrnl, nil, nil, nil, notif, discard)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "Deadwood",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Sequential, not subtests: each check drives the same incident through
	// its next state, and the point is exercising one shared subscriber
	// channel across all three calls, the same as one open SSE connection
	// would see them.
	ch, unsubscribe := jrnl.Subscribe(inc.ID)
	defer unsubscribe()

	if resolveErr := svc.Resolve(ctx, inc.ID); resolveErr != nil {
		t.Fatal(resolveErr)
	}
	waitForStatusChanged(t, ch, "Resolve")

	if rerunErr := svc.Rerun(ctx, inc.ID); rerunErr != nil {
		t.Fatal(rerunErr)
	}
	waitForStatusChanged(t, ch, "Rerun")

	if unlockErr := svc.Unlock(ctx, inc.ID); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	waitForStatusChanged(t, ch, "Unlock")
}

// waitForStatusChanged fails the test if no status_changed event arrives on
// ch within notifyWaitTimeout.
func waitForStatusChanged(t *testing.T, ch <-chan *db.Event, after string) {
	t.Helper()
	select {
	case e := <-ch:
		if e.Kind != string(journal.KindStatusChanged) {
			t.Errorf("after %s: got event kind %q, want %q", after, e.Kind, journal.KindStatusChanged)
		}
	case <-time.After(notifyWaitTimeout):
		t.Fatalf(
			"after %s: timed out waiting for a status_changed event — the live page would never have updated", after,
		)
	}
}

// blockingAgent's Run blocks until its context is cancelled, then returns
// that cancellation as its error — for proving runManager actually cancels
// the context it hands to Agent.Run, through the real Service→runManager→
// Agent.Run path rather than by inspecting runManager's internals directly.
type blockingAgent struct {
	started chan struct{}
	done    chan error
}

func newBlockingAgent() *blockingAgent {
	return &blockingAgent{started: make(chan struct{}, 1), done: make(chan error, 1)}
}

func (a *blockingAgent) Run(
	ctx context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	a.started <- struct{}{}
	<-ctx.Done()
	a.done <- ctx.Err()
	return nil, nil, ctx.Err()
}

func (a *blockingAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	return false
}
func (a *blockingAgent) ScanRunning(_ context.Context) bool { return false }

func (a *blockingAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *blockingAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *blockingAgent) RunEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *blockingAgent) CheckPendingOutcome(
	_ context.Context, _ *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return &agent.PendingOutcomeObservation{}, nil
}

// TestShutdown_CancelsInFlightRun is the regression test for runManager's
// redesign away from storing a base [context.Context] field (the containedctx
// lint): cancellation on process shutdown used to come directly from
// [context.WithCancel](m.base); it now comes from a relay goroutine watching a
// channel that closes once instead. This proves that indirection still
// delivers the original guarantee end to end — cancelling the context
// NewService was constructed with actually reaches a live Agent.Run call,
// through the real Service, not by calling runManager's unexported methods
// directly.
func TestShutdown_CancelsInFlightRun(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	fakeAgent := newBlockingAgent()
	notif := newSyncNotifier()
	svc := incident.NewService(base, database, nil, fakeAgent, nil, nil, notif, slog.New(slog.DiscardHandler))

	if _, handleErr := svc.Handle(context.Background(), &incident.Report{
		Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "Halt and Catch Fire",
	}); handleErr != nil {
		t.Fatal(handleErr)
	}

	select {
	case <-fakeAgent.started:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the run to start")
	}

	cancelBase()

	select {
	case runErr := <-fakeAgent.done:
		if !errors.Is(runErr, context.Canceled) {
			t.Errorf("Run's context error = %v, want context.Canceled", runErr)
		}
	case <-time.After(notifyWaitTimeout):
		t.Fatal("run was not cancelled by shutting down the base context")
	}
}

func TestSetAutonomousPaused(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	paused, _ := database.IsAutonomousPaused(ctx)
	if paused {
		t.Error("should start unpaused")
	}

	if err := svc.SetAutonomousPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	paused, _ = database.IsAutonomousPaused(ctx)
	if !paused {
		t.Error("should be paused")
	}

	if err := svc.SetAutonomousPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	paused, _ = database.IsAutonomousPaused(ctx)
	if paused {
		t.Error("should be unpaused again")
	}
}

// sequencedAgent is a fake AgentRunner whose first Run call simulates a fix that
// needs verification (so the service enters runVerification and blocks there) and
// whose second+ call resolves immediately. It lets a test drive two overlapping
// background runs deterministically instead of relying on sleeps.
type sequencedAgent struct {
	calls               atomic.Int32
	verifyResolvedCalls atomic.Int32
	runCalls            chan int32
}

func newSequencedAgent() *sequencedAgent {
	return &sequencedAgent{runCalls: make(chan int32, 4)}
}

func (a *sequencedAgent) Run(
	_ context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	n := a.calls.Add(1)
	a.runCalls <- n
	if n == 1 {
		// First run: defers to verification with a long delay. Only cancellation
		// (from a superseding run) should end this wait within the test's timeout.
		return &agent.DiagnosticResult{
			RootCause: "test", Confidence: "high",
			PrimaryAction: "run-a", PrimaryReason: "test",
			VerifyAfterSeconds: 30,
		}, nil, nil
	}
	// Second+ run: resolves immediately, no verification needed.
	return &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high",
		PrimaryAction: "run-b", PrimaryReason: "test",
	}, nil, nil
}

func (a *sequencedAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	a.verifyResolvedCalls.Add(1)
	return false
}

func (a *sequencedAgent) ScanRunning(_ context.Context) bool { return false }

func (a *sequencedAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *sequencedAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *sequencedAgent) RunEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *sequencedAgent) CheckPendingOutcome(
	_ context.Context, _ *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return &agent.PendingOutcomeObservation{}, nil
}

// syncNotifier is a Notifier safe for concurrent use (captureNotifier is not),
// needed once a test drives genuinely overlapping goroutines. userMsgs additionally
// lets a test block until a reporter DM arrives instead of polling.
type syncNotifier struct {
	mu       sync.Mutex
	msgs     []string
	userMsgs chan string
}

func newSyncNotifier() *syncNotifier {
	return &syncNotifier{userMsgs: make(chan string, 8)}
}

func (n *syncNotifier) NotifyOwner(_ context.Context, msg string) error {
	n.mu.Lock()
	n.msgs = append(n.msgs, msg)
	n.mu.Unlock()
	return nil
}

func (n *syncNotifier) NotifyUser(_ context.Context, _, msg string) error {
	n.mu.Lock()
	n.msgs = append(n.msgs, msg)
	n.mu.Unlock()
	n.userMsgs <- msg
	return nil
}

const notifyWaitTimeout = 2 * time.Second

// TestRerun_SupersedesInFlightRun_NotifiesReporterExactlyOnce reproduces the
// reported bug directly: rerunning an incident while its first run is still in
// (simulated) verification must cancel that stale run rather than let it race a
// second run to completion. Before the runManager/TransitionStatus fix, both runs
// could independently conclude "fixed" and each DM the reporter once — the
// duplicate "fixed automatically" message. This asserts exactly one DM arrives and
// that the superseded run never got far enough to call VerifyResolved.
func TestRerun_SupersedesInFlightRun_NotifiesReporterExactlyOnce(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	fakeAgent := newSequencedAgent()
	notif := newSyncNotifier()
	svc := incident.NewService(
		context.Background(), database, nil, fakeAgent, nil, nil, notif, slog.New(slog.DiscardHandler),
	)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "Darker Than Black",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for run A to start (Handle launched it in the background).
	select {
	case n := <-fakeAgent.runCalls:
		if n != 1 {
			t.Fatalf("expected first run call, got call #%d", n)
		}
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for run A to start")
	}

	// Rerun must supersede (cancel) run A and launch run B.
	if rerunErr := svc.Rerun(ctx, inc.ID); rerunErr != nil {
		t.Fatal(rerunErr)
	}

	select {
	case n := <-fakeAgent.runCalls:
		if n != 2 {
			t.Fatalf("expected second run call, got call #%d", n)
		}
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for run B to start")
	}

	// Exactly one "fixed" DM should reach the reporter, from run B.
	var dm string
	select {
	case dm = <-notif.userMsgs:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the fixed-notification DM")
	}
	if !strings.Contains(dm, "fixed automatically") {
		t.Errorf("unexpected DM: %q", dm)
	}

	// No second DM should ever arrive — this is the exact bug reported.
	select {
	case second := <-notif.userMsgs:
		t.Fatalf("received a second reporter DM (duplicate notification): %q", second)
	case <-time.After(300 * time.Millisecond):
	}

	// Run A must have been cancelled before its verification loop ever checked
	// VerifyResolved — proves supersession, not a lucky race on the DB gate alone.
	if calls := fakeAgent.verifyResolvedCalls.Load(); calls != 0 {
		t.Errorf("VerifyResolved called %d times; run A should have exited via ctx.Done() first", calls)
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusAgentFixed {
		t.Errorf("final status: got %q want %q", got.Status, db.StatusAgentFixed)
	}
}

// alwaysFixedAgent is a minimal fake AgentRunner whose Run call always resolves
// immediately (no approval, no verification needed).
type alwaysFixedAgent struct {
	runCalls chan struct{}
}

func newAlwaysFixedAgent() *alwaysFixedAgent {
	return &alwaysFixedAgent{runCalls: make(chan struct{}, 4)}
}

func (a *alwaysFixedAgent) Run(
	_ context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	a.runCalls <- struct{}{}
	return &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high",
		PrimaryAction: "test-action", PrimaryReason: "test",
	}, nil, nil
}

func (a *alwaysFixedAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	return true
}
func (a *alwaysFixedAgent) ScanRunning(_ context.Context) bool { return false }

func (a *alwaysFixedAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *alwaysFixedAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *alwaysFixedAgent) RunEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *alwaysFixedAgent) CheckPendingOutcome(
	_ context.Context, _ *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return &agent.PendingOutcomeObservation{}, nil
}

func waitForUserMsg(t *testing.T, n *syncNotifier) string {
	t.Helper()
	select {
	case msg := <-n.userMsgs:
		return msg
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for reporter DM")
		return ""
	}
}

// TestRerun_OfTerminalIncident_ActuallyRunsAgain is the regression test for the
// "Re-investigate does nothing" bug: rerunning an incident that already reached
// a terminal status (agent_fixed) must still produce a second, visible run.
// Before the fix, neither Reopen nor Reinvestigate reset status to something
// Agent.Run's own transition gate would accept from a terminal state, so the
// second run executed but could never transition the incident — no status
// change, no second notification, nothing visible on the dashboard.
func TestRerun_OfTerminalIncident_ActuallyRunsAgain(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	fakeAgent := newAlwaysFixedAgent()
	notif := newSyncNotifier()
	svc := incident.NewService(
		context.Background(), database, nil, fakeAgent, nil, nil, notif, slog.New(slog.DiscardHandler),
	)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "House",
	})
	if err != nil {
		t.Fatal(err)
	}

	// First run resolves and fixes the incident.
	if dm := waitForUserMsg(t, notif); !strings.Contains(dm, "fixed automatically") {
		t.Fatalf("unexpected first DM: %q", dm)
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusAgentFixed {
		t.Fatalf("status after first run: got %q want %q", got.Status, db.StatusAgentFixed)
	}

	// Re-run diagnosis on the now-terminal incident.
	if rerunErr := svc.Rerun(ctx, inc.ID); rerunErr != nil {
		t.Fatal(rerunErr)
	}

	// The second run must actually execute and reach the same terminal status
	// again — proof the rerun was not silently swallowed.
	if dm := waitForUserMsg(t, notif); !strings.Contains(dm, "fixed automatically") {
		t.Fatalf("unexpected second DM: %q", dm)
	}
	got, err = database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusAgentFixed {
		t.Fatalf("status after rerun: got %q want %q", got.Status, db.StatusAgentFixed)
	}
	// Both DMs were sent after their respective Run() calls returned, so by now
	// both sends into the buffered runCalls channel have already landed.
	if n := len(fakeAgent.runCalls); n != 2 {
		t.Fatalf("expected exactly 2 Run calls, channel holds %d", n)
	}
}

// concurrencyTrackingAgent is a fake AgentRunner whose Run call records how
// many Run calls were ever in flight simultaneously across all incidents,
// proving (or disproving) global serialization directly rather than by timing
// inference. A short sleep inside Run widens the window in which a second,
// unserialized Run call would be observed overlapping the first.
type concurrencyTrackingAgent struct {
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	runCalls    chan struct{}
}

func newConcurrencyTrackingAgent() *concurrencyTrackingAgent {
	return &concurrencyTrackingAgent{runCalls: make(chan struct{}, 8)}
}

func (a *concurrencyTrackingAgent) Run(
	_ context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	n := a.inFlight.Add(1)
	for {
		cur := a.maxInFlight.Load()
		if n <= cur || a.maxInFlight.CompareAndSwap(cur, n) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	a.inFlight.Add(-1)
	a.runCalls <- struct{}{}
	return &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high",
		PrimaryAction: "test-action", PrimaryReason: "test",
	}, nil, nil
}

func (a *concurrencyTrackingAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	return true
}
func (a *concurrencyTrackingAgent) ScanRunning(_ context.Context) bool { return false }

func (a *concurrencyTrackingAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *concurrencyTrackingAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *concurrencyTrackingAgent) RunEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *concurrencyTrackingAgent) CheckPendingOutcome(
	_ context.Context, _ *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return &agent.PendingOutcomeObservation{}, nil
}

// TestConcurrentIncidents_DiagnosticRunsAreSerialized reports two different
// incidents at once (Handle launches each in its own background goroutine)
// and asserts the fake agent never observed more than one Run call in flight
// simultaneously — the property runManager.globalSlot exists to guarantee, so
// one incident's evidence-gathering can never interleave with another's
// disruptive action mid-tool-call.
func TestConcurrentIncidents_DiagnosticRunsAreSerialized(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	fakeAgent := newConcurrencyTrackingAgent()
	notif := newSyncNotifier()
	svc := incident.NewService(
		context.Background(), database, nil, fakeAgent, nil, nil, notif, slog.New(slog.DiscardHandler),
	)
	ctx := context.Background()

	const numIncidents = 3
	for i := range numIncidents {
		if _, handleErr := svc.Handle(ctx, &incident.Report{
			Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
			What: "cant_play", Title: fmt.Sprintf("Show %d", i),
		}); handleErr != nil {
			t.Fatal(handleErr)
		}
	}

	for range numIncidents {
		select {
		case <-fakeAgent.runCalls:
		case <-time.After(notifyWaitTimeout):
			t.Fatal("timed out waiting for a Run call")
		}
	}

	if maxObserved := fakeAgent.maxInFlight.Load(); maxObserved != 1 {
		t.Errorf("expected at most 1 concurrent Run call, observed %d", maxObserved)
	}
}

func TestActionAlreadyTried(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "seerr", ReportedBy: "x", What: "cant_play", Title: "T",
	})
	if err != nil {
		t.Fatal(err)
	}

	if svc.ActionAlreadyTriedForTest(ctx, inc.ID, "clear_jellyfin_cache") {
		t.Error("expected no prior action on a fresh incident")
	}

	if logErr := database.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied,
	}); logErr != nil {
		t.Fatal(logErr)
	}

	if !svc.ActionAlreadyTriedForTest(ctx, inc.ID, "clear_jellyfin_cache") {
		t.Error("expected the logged action to be detected as already tried")
	}
	if svc.ActionAlreadyTriedForTest(ctx, inc.ID, "restart_jellyfin") {
		t.Error("a different action should not be flagged as already tried")
	}
}

// TestReviewRiskReason is the unit-level regression test for the Phase 4
// trigger conditions: a diagnosis the model itself never flags
// requires_approval for must still be flagged for control review when its
// confidence is low, or when it repeats an action already applied on this
// incident — the two conditions that, before this, were never checked at
// all, leaving the control reviewer permanently unreachable in production.
func TestReviewRiskReason(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "seerr", ReportedBy: "x", What: "cant_play", Title: "T",
	})
	if err != nil {
		t.Fatal(err)
	}

	fresh := &agent.DiagnosticResult{Confidence: "high", PrimaryAction: "clear_jellyfin_cache"}
	if got := svc.ReviewRiskReasonForTest(ctx, inc.ID, fresh); got != "" {
		t.Errorf("expected no risk reason for a fresh, confident diagnosis; got %q", got)
	}

	low := &agent.DiagnosticResult{Confidence: "low", PrimaryAction: "clear_jellyfin_cache"}
	if got := svc.ReviewRiskReasonForTest(ctx, inc.ID, low); got == "" {
		t.Error("expected a risk reason for a low-confidence diagnosis")
	}

	if logErr := database.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied,
	}); logErr != nil {
		t.Fatal(logErr)
	}
	repeat := &agent.DiagnosticResult{Confidence: "high", PrimaryAction: "clear_jellyfin_cache"}
	if got := svc.ReviewRiskReasonForTest(ctx, inc.ID, repeat); got == "" {
		t.Error("expected a risk reason for repeating an already-applied action")
	}
}

// TestReviewRiskReason_LadderClimbing is the regression test for the gap
// exact-repeat detection can't catch: each rung of a ladder-climb is a NEW
// action (clear cache, then scan, then restart), so actionAlreadyTried never
// fires even though the pattern is exactly what the systemPrompt says not to
// do. Two distinct already-applied actions must be enough to force review of
// a third, confident, never-before-tried action.
func TestReviewRiskReason_LadderClimbing(t *testing.T) {
	t.Parallel()
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "seerr", ReportedBy: "x", What: "cant_play", Title: "Doctor Who",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Only one distinct action applied so far: a fresh, confident, different
	// third action should NOT trigger review on the ladder-climbing rule yet.
	if logErr := database.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied,
	}); logErr != nil {
		t.Fatal(logErr)
	}
	second := &agent.DiagnosticResult{Confidence: "high", PrimaryAction: "jellyfin_library_scan"}
	if got := svc.ReviewRiskReasonForTest(ctx, inc.ID, second); got != "" {
		t.Errorf("expected no risk reason with only 1 distinct prior action, got %q", got)
	}

	// Two distinct actions applied: a third, fresh, confident, never-tried
	// action must now trigger review even though it isn't itself a repeat.
	if logErr := database.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "jellyfin_library_scan", TriggeredBy: "agent", Status: db.ActionApplied,
	}); logErr != nil {
		t.Fatal(logErr)
	}
	third := &agent.DiagnosticResult{Confidence: "high", PrimaryAction: "restart_jellyfin"}
	got := svc.ReviewRiskReasonForTest(ctx, inc.ID, third)
	if got == "" {
		t.Error("expected a risk reason once 2+ distinct actions have already been tried")
	}
	if !strings.Contains(got, "2 different actions") {
		t.Errorf("expected the reason to name the count, got %q", got)
	}

	if n := svc.DistinctAppliedActionCountForTest(ctx, inc.ID); n != 2 {
		t.Errorf("distinct applied action count: got %d want 2", n)
	}
}

func TestControlProposal(t *testing.T) {
	t.Parallel()

	approval := &agent.DiagnosticResult{
		RequiresApproval: true, EscalateAction: "manual_investigation",
	}
	if got := incident.ControlProposalForTest(approval, "", ""); !strings.Contains(got, "escalate") {
		t.Errorf("expected an escalation-flavored proposal, got %q", got)
	}

	autonomous := &agent.DiagnosticResult{
		PrimaryAction: "clear_jellyfin_cache", RootCause: "stale metadata",
	}
	got := incident.ControlProposalForTest(
		autonomous, `"clear_jellyfin_cache" was already applied`,
		"\nActions already applied on this incident (none resolved it): jellyfin_library_scan",
	)
	if !strings.Contains(got, "clear_jellyfin_cache") || !strings.Contains(got, "already applied") {
		t.Errorf("expected the proposal to name the action and the risk reason, got %q", got)
	}
	if !strings.Contains(got, "jellyfin_library_scan") {
		t.Errorf("expected the proposal to include the action history, got %q", got)
	}
}

// fakeControlLLMServer serves the /chat/completions shape go-openai expects,
// with the review verdict baked into the message content.
func fakeControlLLMServer(t *testing.T, verdictJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "test", "object": "chat.completion", "created": 0, "model": "test",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": verdictJSON},
				"finish_reason": "stop",
			}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// lowConfidenceOnceAgent is a minimal fake AgentRunner whose Run call always
// returns a low-confidence, non-approval diagnosis — Phase 4's
// "review on low confidence" trigger.
type lowConfidenceOnceAgent struct {
	runCalls chan struct{}
}

func newLowConfidenceOnceAgent() *lowConfidenceOnceAgent {
	return &lowConfidenceOnceAgent{runCalls: make(chan struct{}, 4)}
}

func (a *lowConfidenceOnceAgent) Run(
	_ context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	a.runCalls <- struct{}{}
	result := &agent.DiagnosticResult{
		RootCause: "test", Confidence: "low",
		PrimaryAction: "clear_jellyfin_cache", PrimaryReason: "test",
	}
	conversation := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "test conversation"},
	}
	return result, conversation, nil
}

func (a *lowConfidenceOnceAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	return true
}
func (a *lowConfidenceOnceAgent) ScanRunning(_ context.Context) bool { return false }

func (a *lowConfidenceOnceAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *lowConfidenceOnceAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *lowConfidenceOnceAgent) RunEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *lowConfidenceOnceAgent) CheckPendingOutcome(
	_ context.Context, _ *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return &agent.PendingOutcomeObservation{}, nil
}

// TestRunAgent_LowConfidence_TriggersControlReview_ApproveProceedsAutonomously
// is the end-to-end regression test for Phase 4: before this, control review
// only ever ran when the model itself set requires_approval — which it did
// zero times across a week of production traffic, so the reviewer never once
// executed. This confirms a low-confidence-but-autonomous diagnosis is
// routed through review, and that an "approve" verdict on it proceeds
// autonomously (a "fixed" DM) rather than being surfaced to the owner as if
// it were a genuine approval request.
func TestRunAgent_LowConfidence_TriggersControlReview_ApproveProceedsAutonomously(t *testing.T) {
	t.Parallel()

	srv := fakeControlLLMServer(t, `{"verdict":"approve","reason":"looks fine"}`)

	llmCfg := openai.DefaultConfig("test-key")
	llmCfg.BaseURL = srv.URL
	control := agent.NewControlReviewer(openai.NewClientWithConfig(llmCfg), "test-model", slog.New(slog.DiscardHandler))

	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	fakeAgent := newLowConfidenceOnceAgent()
	notif := newSyncNotifier()
	svc := incident.NewService(
		context.Background(), database, nil, fakeAgent, control, nil, notif, slog.New(slog.DiscardHandler),
	)
	ctx := context.Background()

	if _, err = svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "Low Confidence Show",
	}); err != nil {
		t.Fatal(err)
	}

	dm := waitForUserMsg(t, notif)
	if !strings.Contains(dm, "fixed automatically") {
		t.Fatalf("expected an autonomous fix notification once control review approved, got: %q", dm)
	}
	if n := len(fakeAgent.runCalls); n != 1 {
		t.Fatalf("expected exactly 1 Run call, channel holds %d", n)
	}
}

// failingUserNotifier always fails NotifyUser (simulating a Discord "no
// mutual guilds" DM failure) but records NotifyOwner calls, to test the
// reporter-DM fallback path.
type failingUserNotifier struct{ ownerMsgs []string }

func (n *failingUserNotifier) NotifyOwner(_ context.Context, msg string) error {
	n.ownerMsgs = append(n.ownerMsgs, msg)
	return nil
}

func (n *failingUserNotifier) NotifyUser(_ context.Context, _, _ string) error {
	return errors.New("no mutual guilds")
}

// TestNotifyReporters_FailedDMFallsBackToOwner is the regression test for a
// reporter DM that fails silently: before this, a failed NotifyUser call was
// only ever ERROR-logged — the reporter was never told anything, and no one
// else was either.
func TestNotifyReporters_FailedDMFallsBackToOwner(t *testing.T) {
	t.Parallel()
	notif := &failingUserNotifier{}

	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	svc := incident.NewService(context.Background(), database, nil, nil, nil, nil, notif, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "House",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.NotifyReportersForTest(ctx, inc, "✅ your report has been fixed")

	if len(notif.ownerMsgs) != 1 {
		t.Fatalf("expected 1 owner fallback message, got %d: %v", len(notif.ownerMsgs), notif.ownerMsgs)
	}
	if !strings.Contains(notif.ownerMsgs[0], "discord-alice") || !strings.Contains(notif.ownerMsgs[0], "House") {
		t.Errorf("owner fallback message missing reporter/incident context: %q", notif.ownerMsgs[0])
	}
}

// TestSweepStaleRuns_RerunsHungIncident is the regression test for the
// heartbeat/stale-run detection gap named in ROADMAP.md: RecoverZombies only
// catches a crashed process (nothing left running to check); this is what
// catches a run whose goroutine is still alive but stopped making progress.
func TestSweepStaleRuns_RerunsHungIncident(t *testing.T) {
	t.Parallel()

	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	dbPath := f.Name()

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Hung"}
	if createErr := database.CreateIncident(ctx, inc); createErr != nil {
		t.Fatal(createErr)
	}
	if statusErr := database.UpdateIncidentStatus(ctx, inc.ID, db.StatusInvestigating); statusErr != nil {
		t.Fatal(statusErr)
	}

	// Backdate updated_at (no heartbeat written yet) well past the stale
	// threshold, simulating a run whose goroutine hung mid-diagnosis — via a
	// second raw connection, since db.DB exposes no "set an arbitrary past
	// timestamp" method (nor should it, outside a test).
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, execErr := raw.ExecContext(ctx, `UPDATE incidents SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-20*time.Minute), inc.ID); execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr := raw.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	fakeAgent := newAlwaysFixedAgent()
	notif := newSyncNotifier()
	svc := incident.NewService(
		context.Background(), database, nil, fakeAgent, nil, nil, notif, slog.New(slog.DiscardHandler),
	)

	svc.SweepStaleRuns(ctx)

	select {
	case <-fakeAgent.runCalls:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the stale run to be rerun")
	}
}
