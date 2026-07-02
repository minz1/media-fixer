package incident_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
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
	svc := incident.NewService(context.Background(), database, nil, nil, nil, notif, slog.New(slog.DiscardHandler))
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

func TestReopenClearsLock(t *testing.T) {
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

	// Reopen is a deliberate human override — it must clear the lock.
	if reopenErr := svc.Reopen(ctx, inc.ID); reopenErr != nil {
		t.Fatal(reopenErr)
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutonomousLocked {
		t.Error("reopen should clear the autonomous lock")
	}
	if got.Status != db.StatusReopened {
		t.Errorf("status after reopen: %q", got.Status)
	}
}

func TestResolveAndReopen(t *testing.T) {
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

	// Reopen with nil agent just marks it reopened (agent goroutine exits immediately).
	if reopenErr := svc.Reopen(ctx, inc.ID); reopenErr != nil {
		t.Fatal(reopenErr)
	}
	got, err = database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusReopened {
		t.Errorf("status after reopen: %q", got.Status)
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

func (a *sequencedAgent) VerifyResolved(_ context.Context, _ string) bool {
	a.verifyResolvedCalls.Add(1)
	return false
}

func (a *sequencedAgent) ScanRunning(_ context.Context) bool { return false }

func (a *sequencedAgent) BuildSummarySeed(_ *db.Incident, _ string) []openai.ChatCompletionMessage {
	return nil
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

// TestReopen_SupersedesInFlightRun_NotifiesReporterExactlyOnce reproduces the
// reported bug directly: reopening an incident while its first run is still in
// (simulated) verification must cancel that stale run rather than let it race a
// second run to completion. Before the runManager/TransitionStatus fix, both runs
// could independently conclude "fixed" and each DM the reporter once — the
// duplicate "fixed automatically" message. This asserts exactly one DM arrives and
// that the superseded run never got far enough to call VerifyResolved.
func TestReopen_SupersedesInFlightRun_NotifiesReporterExactlyOnce(t *testing.T) {
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
		context.Background(), database, fakeAgent, nil, nil, notif, slog.New(slog.DiscardHandler),
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

	// Reopen must supersede (cancel) run A and launch run B.
	if reopenErr := svc.Reopen(ctx, inc.ID); reopenErr != nil {
		t.Fatal(reopenErr)
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
