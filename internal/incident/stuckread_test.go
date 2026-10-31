package incident_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

type fakeStuckChecker struct {
	mu     sync.Mutex
	clear  func(call int) bool
	titles []string
	times  []time.Time
}

func (c *fakeStuckChecker) Clear(title string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.titles = append(c.titles, title)
	c.times = append(c.times, time.Now())
	return c.clear(len(c.titles))
}

func (c *fakeStuckChecker) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.titles)
}

type stuckReadAgent struct {
	*lowConfidenceOnceAgent

	t       *testing.T
	mu      sync.Mutex
	ranAt   time.Time
	scans   int
	started chan string
}

func newStuckReadAgent(t *testing.T) *stuckReadAgent {
	return &stuckReadAgent{
		lowConfidenceOnceAgent: newLowConfidenceOnceAgent(), t: t, started: make(chan string, 8),
	}
}

func (a *stuckReadAgent) Run(
	_ context.Context, inc *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	a.mu.Lock()
	a.ranAt = time.Now()
	a.mu.Unlock()
	a.started <- inc.ID
	return &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high", PrimaryAction: "run-a", PrimaryReason: "test",
	}, nil, nil
}

func (a *stuckReadAgent) VerifyResolved(context.Context, string, string, *agent.FixSignature) bool {
	a.t.Error("VerifyResolved must not be called for stuck_read")
	return true
}

func (a *stuckReadAgent) ScanRunning(context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scans++
	return true
}

func (a *stuckReadAgent) scanCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scans
}

func newStuckReadService(
	t *testing.T, ag incident.AgentRunner, checks int, unit time.Duration,
) (*incident.Service, *db.DB) {
	t.Helper()
	database := openTimersDB(t)
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.VerifyChecks = checks
		tm.VerifyGapMinutes = 1
	})
	svc := incident.NewService(
		t.Context(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.DiscardHandler),
	)
	svc.SetUnitForTest(unit)
	return svc, database
}

func handleKind(t *testing.T, svc *incident.Service, what, title string) *db.Incident {
	t.Helper()
	inc, err := svc.Handle(t.Context(), &incident.Report{
		Source: "media-agent", ReportedBy: "media-agent", What: what, Title: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func waitForStatus(t *testing.T, database *db.DB, id string, want db.IncidentStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := database.GetIncident(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %q, want %q", got.Status, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStuckReadFixWaitsForClear(t *testing.T) {
	t.Parallel()
	ag := newStuckReadAgent(t)
	svc, database := newStuckReadService(t, ag, 3, 20*time.Millisecond)
	chk := &fakeStuckChecker{clear: func(n int) bool { return n >= 2 }}
	svc.SetStuckRead(chk)

	inc := handleKind(t, svc, agent.WhatStuckRead, "Stuck Show")
	<-ag.started
	waitForStatus(t, database, inc.ID, db.StatusVerifying)
	waitForStatus(t, database, inc.ID, db.StatusAgentFixed)
	if n := chk.calls(); n != 2 {
		t.Fatalf("Clear calls = %d, want 2", n)
	}
	if chk.titles[0] != "Stuck Show" {
		t.Fatalf("Clear title = %q", chk.titles[0])
	}
}

func TestStuckReadVerifyFloor(t *testing.T) {
	t.Parallel()
	const unit = 50 * time.Millisecond
	ag := newStuckReadAgent(t)
	svc, database := newStuckReadService(t, ag, 1, unit)
	chk := &fakeStuckChecker{clear: func(int) bool { return true }}
	svc.SetStuckRead(chk)

	inc := handleKind(t, svc, agent.WhatStuckRead, "Floor Show")
	waitForStatus(t, database, inc.ID, db.StatusAgentFixed)
	ag.mu.Lock()
	ranAt := ag.ranAt
	ag.mu.Unlock()
	chk.mu.Lock()
	defer chk.mu.Unlock()
	if len(chk.times) != 1 {
		t.Fatalf("Clear calls = %d, want 1", len(chk.times))
	}
	if d := chk.times[0].Sub(ranAt); d < 3*unit {
		t.Fatalf("first Clear %v after Run, want at least %v", d, 3*unit)
	}
}

func TestStuckReadNeverClearEscalates(t *testing.T) {
	t.Parallel()
	ag := newStuckReadAgent(t)
	svc, database := newStuckReadService(t, ag, 2, time.Millisecond)
	chk := &fakeStuckChecker{clear: func(int) bool { return false }}
	svc.SetStuckRead(chk)

	inc := handleKind(t, svc, agent.WhatStuckRead, "Never Show")
	waitForStatus(t, database, inc.ID, db.StatusManualTestNeeded)
	if n := chk.calls(); n != 2 {
		t.Fatalf("Clear calls = %d, want the configured 2", n)
	}
	if n := ag.scanCalls(); n != 0 {
		t.Fatalf("ScanRunning calls = %d, want 0", n)
	}
}

func TestStuckReadNoChecker(t *testing.T) {
	t.Parallel()
	ag := newStuckReadAgent(t)
	svc, database := newStuckReadService(t, ag, 2, time.Millisecond)

	inc := handleKind(t, svc, agent.WhatStuckRead, "Nochecker Show")
	waitForStatus(t, database, inc.ID, db.StatusManualTestNeeded)
	if n := ag.scanCalls(); n != 0 {
		t.Fatalf("ScanRunning calls = %d, want 0", n)
	}
}

func TestRecoverZombiesRerunsVerifyingStuckRead(t *testing.T) {
	t.Parallel()
	ag := newStuckReadAgent(t)
	svc, database := newStuckReadService(t, ag, 2, time.Hour)
	mk := func(what, title string) *db.Incident {
		inc := &db.Incident{Status: db.StatusOpen, Source: "media-agent", ReportedBy: "x", What: what, Title: title}
		if err := database.CreateIncident(t.Context(), inc); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateIncidentStatus(t.Context(), inc.ID, db.StatusVerifying); err != nil {
			t.Fatal(err)
		}
		return inc
	}
	stuck := mk(agent.WhatStuckRead, "Recover Stuck")
	cant := mk("cant_play", "Recover Cant")

	svc.RecoverZombies(t.Context())

	select {
	case id := <-ag.started:
		if id != stuck.ID {
			t.Fatalf("rerun incident %s, want the stuck_read one %s", id, stuck.ID)
		}
	case <-time.After(timersLate):
		t.Fatal("no run launched for the verifying stuck_read incident")
	}
	select {
	case id := <-ag.started:
		t.Fatalf("unexpected second run for incident %s", id)
	case <-time.After(timersEarly):
	}
	got, err := database.GetIncident(t.Context(), cant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusVerifying {
		t.Fatalf("cant_play status %q, want untouched verifying", got.Status)
	}
}

func TestCantPlayStillImmediate(t *testing.T) {
	t.Parallel()
	ag := newStuckReadAgent(t)
	svc, database := newStuckReadService(t, ag, 2, time.Millisecond)
	chk := &fakeStuckChecker{clear: func(int) bool { return false }}
	svc.SetStuckRead(chk)

	inc := handleKind(t, svc, "cant_play", "Plain Show")
	waitForStatus(t, database, inc.ID, db.StatusAgentFixed)
	if n := chk.calls(); n != 0 {
		t.Fatalf("Clear calls = %d, want 0", n)
	}
}
