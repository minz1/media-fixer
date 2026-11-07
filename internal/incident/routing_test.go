package incident_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
)

type gateAgent struct {
	*lowConfidenceOnceAgent

	d       *db.DB
	started chan string
	runs    atomic.Int32
	behave  func(ctx context.Context, gate agent.Gate) agent.RunExit
}

func newGateAgent(behave func(ctx context.Context, gate agent.Gate) agent.RunExit) *gateAgent {
	return &gateAgent{
		lowConfidenceOnceAgent: newLowConfidenceOnceAgent(),
		started:                make(chan string, 8),
		behave:                 behave,
	}
}

func (a *gateAgent) Run(
	ctx context.Context, inc *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	if err := gate.Claim(ctx); err != nil {
		if errors.Is(err, agent.ErrIncidentNotInvestigatable) {
			return agent.RunExit{State: agent.RunNotClaimed, Err: err}
		}
		return agent.RunExit{State: agent.RunClaimFailed, Err: err}
	}
	a.runs.Add(1)
	_ = a.d.AppendEvents(context.WithoutCancel(ctx),
		&db.Event{IncidentID: inc.ID, Kind: string(journal.KindRunStarted), Payload: json.RawMessage(`{}`)})
	a.started <- inc.ID
	exit := a.behave(ctx, gate)
	exit.Started = true
	exit.Journal = journal.RunFinishedEvent(inc.ID, 1, string(exit.State), exit.Err)
	return exit
}

func untilVerdict(ctx context.Context, gate agent.Gate, want agent.Verdict) bool {
	deadline := time.Now().Add(notifyWaitTimeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if gate.Live(ctx) == want {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

func confident() *agent.DiagnosticResult {
	return classified(
		&agent.DiagnosticResult{RootCause: "x", Confidence: "high", PrimaryAction: "run-a", PrimaryReason: "x"},
		nil,
	)
}

func lowConfidence() *agent.DiagnosticResult {
	return classified(
		&agent.DiagnosticResult{RootCause: "x", Confidence: "low", PrimaryAction: "run-a", PrimaryReason: "x"},
		nil,
	)
}

func controlServer(t *testing.T, onReview func(), verdict string) *agent.ControlReviewer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if onReview != nil {
			onReview()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "test", "object": "chat.completion", "created": 0, "model": "test",
			"choices": []map[string]any{{
				"index": 0, "message": map[string]any{"role": "assistant", "content": verdict}, "finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(srv.Close)
	cfg := openai.DefaultConfig("k")
	cfg.BaseURL = srv.URL
	return agent.NewControlReviewer(openai.NewClientWithConfig(cfg), "m", slog.New(slog.DiscardHandler))
}

func newRoutingEnv(t *testing.T, ag *gateAgent, control *agent.ControlReviewer, timers func(*incident.Timers)) *lcEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if timers != nil {
		saveTimers(t, database, timers)
	}
	ag.d = database
	env := &lcEnv{path: path, db: database, notif: &recNotifier{}, logs: &syncBuffer{}}
	logger := slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	env.svc = incident.NewService(t.Context(), database, nil, ag, control, nil, env.notif, logger)
	t.Cleanup(func() { stopService(t, env.svc) })
	env.svc.SetUnitForTest(time.Millisecond)
	return env
}

func (e *lcEnv) report(t *testing.T, title string) string {
	t.Helper()
	inc, err := e.svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "u-alice", What: "cant_play", Title: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	return inc.ID
}

func (e *lcEnv) kinds(t *testing.T, id string) []string {
	t.Helper()
	events, err := e.db.EventsSince(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

func (e *lcEnv) count(t *testing.T, id, kind string) int {
	t.Helper()
	n := 0
	for _, k := range e.kinds(t, id) {
		if k == kind {
			n++
		}
	}
	return n
}

func waitGateStart(t *testing.T, ag *gateAgent) {
	t.Helper()
	select {
	case <-ag.started:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("the run never started")
	}
}

func TestA5_3_PauseStopsInFlightRunAtNextAction(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(func(ctx context.Context, gate agent.Gate) agent.RunExit {
		if !untilVerdict(ctx, gate, agent.GatePaused) {
			return agent.RunExit{State: agent.RunSuperseded, Err: ctx.Err()}
		}
		return agent.RunExit{State: agent.RunPaused}
	})
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Pause mid run")
	waitGateStart(t, ag)
	if err := env.svc.SetAutonomousPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	var s lifecycle.Snapshot
	eventually(t, "reopened and parked", func() bool {
		s = env.snap(t, id)
		return s.Status == lifecycle.Reopened && s.Parked
	})
	if env.count(t, id, "run_finished") != 1 || len(env.notif.ownerCards()) != 0 {
		t.Fatalf("events %v, owner cards %d", env.kinds(t, id), len(env.notif.ownerCards()))
	}
}

func TestA5_4_PauseStopsRetry(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(func(context.Context, agent.Gate) agent.RunExit {
		return agent.RunExit{State: agent.RunConcluded, Finding: lowConfidence()}
	})
	paused := make(chan struct{})
	control := controlServer(
		t,
		func() { <-paused },
		`{"verdict":"suggest_alternative","alternative_action":"x","reason":"y"}`,
	)
	env := newRoutingEnv(t, ag, control, nil)
	id := env.report(t, "Pause before retry")
	waitGateStart(t, ag)
	if err := env.svc.SetAutonomousPaused(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	close(paused)
	eventually(t, "reopened and parked", func() bool {
		s := env.snap(t, id)
		return s.Status == lifecycle.Reopened && s.Parked
	})
	if n := ag.runs.Load(); n != 1 {
		t.Fatalf("%d runs, want the retry dropped while paused", n)
	}
}

func blockOnCtx(ctx context.Context, _ agent.Gate) agent.RunExit {
	<-ctx.Done()
	return agent.RunExit{State: agent.RunSuperseded, Err: ctx.Err()}
}

func TestI1_14_ResolveCancelsRun(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Resolve mid run")
	waitGateStart(t, ag)
	if _, err := env.svc.ApplyForTest(t.Context(), id, lifecycle.Event{Kind: lifecycle.OwnerResolve}); err != nil {
		t.Fatal(err)
	}
	resolved := env.snap(t, id)
	eventually(t, "run_finished", func() bool { return env.count(t, id, "run_finished") == 1 })
	got := env.snap(t, id)
	if got.Status != lifecycle.Resolved || got.Version != resolved.Version || len(got.Finding.JSON) != 0 {
		t.Fatalf("a write landed after Resolve: %+v", got)
	}
}

func TestI5_9_ResolveSupersedesRun(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	ag := newGateAgent(func(context.Context, agent.Gate) agent.RunExit {
		<-release
		return agent.RunExit{State: agent.RunConcluded, Finding: confident()}
	})
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Late conclusion")
	waitGateStart(t, ag)
	if _, err := env.svc.ApplyForTest(t.Context(), id, lifecycle.Event{Kind: lifecycle.OwnerResolve}); err != nil {
		t.Fatal(err)
	}
	close(release)
	eventually(t, "run_finished", func() bool { return env.count(t, id, "run_finished") == 1 })
	eventually(t, "the late outcome to be refused", func() bool {
		return env.logged("run outcome not applied, incident already moved on") ||
			env.logged("run outcome dropped, the run was stopped")
	})
	if got := env.snap(t, id); got.Status != lifecycle.Resolved {
		t.Fatalf("status %s", got.Status)
	}
	for _, c := range env.notif.userCards() {
		if strings.Contains(c.Plain(), "fixed automatically") {
			t.Fatal("a superseded run told the reporter it was fixed")
		}
	}
}

func TestAttemptFinished_BeforeControlReview(t *testing.T) {
	t.Parallel()
	var stored atomic.Bool
	var env *lcEnv
	var id atomic.Value
	control := controlServer(t, func() {
		s, err := env.db.GetIncidentSnapshot(context.Background(), id.Load().(string))
		stored.Store(err == nil && len(s.Finding.JSON) > 0 && env.count(t, s.ID, "run_finished") == 1)
	}, `{"verdict":"approve","reason":"ok"}`)
	ready := make(chan struct{})
	ag := newGateAgent(func(context.Context, agent.Gate) agent.RunExit {
		<-ready
		return agent.RunExit{State: agent.RunConcluded, Finding: lowConfidence()}
	})
	env = newRoutingEnv(t, ag, control, nil)
	id.Store(env.report(t, "Review after finding"))
	close(ready)
	eventually(t, "agent fixed", func() bool { return env.snap(t, id.Load().(string)).Status == lifecycle.AgentFixed })
	if !stored.Load() {
		t.Fatal("the control review ran before the finding and run_finished were stored")
	}
}

func TestAttemptFinished_SigtermCutRunStillRecorded(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(blockOnCtx)
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Sigterm")
	waitGateStart(t, ag)
	stopService(t, env.svc)
	if env.count(t, id, "run_finished") != 1 {
		t.Fatalf("events %v, want run_finished recorded for the cut run", env.kinds(t, id))
	}
	if got := env.snap(t, id); got.Status != lifecycle.Investigating || len(env.notif.ownerCards()) != 0 {
		t.Fatalf("a shutdown-cut run escalated: %s", got.Status)
	}
}

func TestRun_NoDanglingRunStartedAcrossRetries(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(func(context.Context, agent.Gate) agent.RunExit {
		return agent.RunExit{State: agent.RunConcluded, Finding: lowConfidence()}
	})
	control := controlServer(t, nil, `{"verdict":"suggest_alternative","alternative_action":"x","reason":"y"}`)
	env := newRoutingEnv(t, ag, control, func(tm *incident.Timers) {
		tm.RetryFirstMinutes, tm.RetrySecondMinutes = 1, 1
	})
	id := env.report(t, "Retries")
	eventuallyWithin(t, loadedWaitTimeout, "the escalation", func() bool {
		return env.snap(t, id).Status == lifecycle.ManualTestNeeded
	})
	if started, finished := env.count(t, id, "run_started"), env.count(t, id, "run_finished"); started != 3 ||
		finished != 3 {
		t.Fatalf("%d run_started, %d run_finished", started, finished)
	}
}

func TestRun_BudgetTripEscalatesBudgetSpent(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(func(context.Context, agent.Gate) agent.RunExit {
		return agent.RunExit{State: agent.RunBudgetTripped, Finding: &agent.DiagnosticResult{
			RootCause: "limit", EscalateAction: agent.EscalateManualInvestigation, RequiresApproval: true,
		}}
	})
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Budget")
	var s lifecycle.Snapshot
	eventually(t, "the escalation", func() bool {
		s = env.snap(t, id)
		return s.Status == lifecycle.ManualTestNeeded
	})
	if s.Reason != lifecycle.ReasonBudgetSpent {
		t.Fatalf("reason %q", s.Reason)
	}
	eventually(t, "the owner card", func() bool {
		cards := env.notif.ownerCards()
		return len(cards) == 1 && strings.Contains(cards[0].Plain(), "needs your attention")
	})
}

func TestShutdownCancelledRun_QuietThenSystemRerun(t *testing.T) {
	t.Parallel()
	ag := newGateAgent(func(ctx context.Context, gate agent.Gate) agent.RunExit {
		for range 2 {
			if v, err := gate.Act(ctx, "clear_jellyfin_cache", nil); err != nil || v != agent.GateAllowed {
				return agent.RunExit{State: agent.RunBatchFailed, Err: err}
			}
		}
		return blockOnCtx(ctx, gate)
	})
	env := newRoutingEnv(t, ag, nil, nil)
	id := env.report(t, "Restart")
	waitGateStart(t, ag)
	eventually(t, "two counted actions", func() bool { return env.snap(t, id).ActionCount == 2 })
	stopService(t, env.svc)
	if got := env.snap(t, id); got.Status != lifecycle.Investigating || len(env.notif.ownerCards()) != 0 {
		t.Fatalf("the cut run was not quiet: %s, %d owner cards", got.Status, len(env.notif.ownerCards()))
	}
	next := incident.NewService(t.Context(), env.db, nil, ag, nil, nil, env.notif, env.svc.LogForTest())
	t.Cleanup(func() { stopService(t, next) })
	next.StartRecovery()
	waitGateStart(t, ag)
	if got := env.snap(t, id); got.ActionCount != 2 || len(env.notif.ownerCards()) != 0 {
		t.Fatalf("restart rerun: count %d, %d owner cards", got.ActionCount, len(env.notif.ownerCards()))
	}
}

type bumpingAgent struct {
	*observingAgent

	t  *testing.T
	db *db.DB
}

func (a *bumpingAgent) Run(
	ctx context.Context, inc *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	exit := gatedExit(ctx, gate, &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high", PrimaryAction: "run-a", PrimaryReason: "test",
	}, nil, nil)
	lifecycletest.Force(a.t, a.db, inc.ID, func(n *lifecycle.Snapshot) { n.Generation++ })
	return exit
}

func TestRunFinished_UsesLaunchGeneration(t *testing.T) {
	t.Parallel()
	ag := &bumpingAgent{observingAgent: newObservingAgent(), t: t}
	env := newLifecycleEnv(t, ag)
	ag.db = env.db
	s := lifecycletest.Seed(t, env.db, lifecycletest.Spec{Title: "Bumped run", State: lifecycle.Open})
	inc, err := env.db.GetIncident(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	inc.Generation = s.Generation + 1
	env.svc.RunAgentAtForTest(t.Context(), inc, s.Generation)
	got := env.snap(t, s.ID)
	if got.Status != lifecycle.Investigating || got.Generation != s.Generation+1 {
		t.Fatalf("a run launched at gen %d finished on gen %d: %s", s.Generation, got.Generation, got.Status)
	}
	if !env.logged("run outcome not applied, incident already moved on") {
		t.Fatalf("stale outcome not logged:\n%s", env.logs.String())
	}
}

func TestGate_RecordsAcrossAttempts(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	var mu sync.Mutex
	var before [][]string
	ag := newGateAgent(func(_ context.Context, gate agent.Gate) agent.RunExit {
		n := attempts.Add(1)
		var tools []string
		for _, rec := range gate.Calls() {
			tools = append(tools, rec.Tool)
		}
		mu.Lock()
		before = append(before, tools)
		mu.Unlock()
		gate.Record(agent.CallRecord{Tool: fmt.Sprintf("tool_%d", n), Counted: true})
		if n == 1 {
			return agent.RunExit{State: agent.RunConcluded, Finding: lowConfidence()}
		}
		return agent.RunExit{State: agent.RunConcluded, Finding: confident()}
	})
	control := controlServer(t, nil, `{"verdict":"suggest_alternative","alternative_action":"x","reason":"y"}`)
	env := newRoutingEnv(t, ag, control, nil)
	env.report(t, "Records across attempts")
	eventually(t, "two attempts", func() bool { return attempts.Load() >= 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(before) < 2 || len(before[0]) != 0 || !slices.Equal(before[1], []string{"tool_1"}) {
		t.Fatalf("records seen at each attempt start = %v, want [] then [tool_1]", before)
	}
}
