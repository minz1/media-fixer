package incident_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
)

const (
	timersUnit      = 10 * time.Millisecond
	timersEarly     = 150 * time.Millisecond
	timersLate      = 2 * time.Second
	timersSeedAlive = 100 * time.Millisecond
)

func openTimersDB(t *testing.T) *db.DB {
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
	return database
}

func saveTimers(t *testing.T, database *db.DB, edit func(*incident.Timers)) {
	t.Helper()
	tm := incident.DefaultTimers()
	edit(&tm)
	if errs := tm.Validate(); len(errs) > 0 {
		t.Fatalf("test row is invalid: %v", errs)
	}
	if err := incident.SaveTimers(t.Context(), database, tm); err != nil {
		t.Fatal(err)
	}
}

func TestRetryDelay_FromSavedTimers(t *testing.T) {
	t.Parallel()
	srv := fakeControlLLMServer(t, `{"verdict":"suggest_alternative","reason":"try again"}`)
	llmCfg := openai.DefaultConfig("test-key")
	llmCfg.BaseURL = srv.URL
	control := agent.NewControlReviewer(openai.NewClientWithConfig(llmCfg), "test-model", slog.New(slog.DiscardHandler))
	database := openTimersDB(t)
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.RetryFirstMinutes = 30
		tm.StaleRunMinutes = 60
	})
	ag := newLowConfidenceOnceAgent()
	svc := incident.NewService(
		t.Context(), database, nil, ag, control, nil, newSyncNotifier(), slog.New(slog.DiscardHandler),
	)
	svc.SetUnitForTest(timersUnit)

	if _, err := svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "Retry Show",
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ag.runCalls:
	case <-time.After(timersLate):
		t.Fatal("first Run never arrived")
	}
	select {
	case <-ag.runCalls:
		t.Fatal("second Run arrived before the saved 30 minute wait elapsed")
	case <-time.After(timersEarly):
	}
	select {
	case <-ag.runCalls:
	case <-time.After(timersLate):
		t.Fatal("second Run never arrived")
	}
}

type countingVerifyAgent struct {
	*lowConfidenceOnceAgent

	verifyCalls atomic.Int32
}

func (a *countingVerifyAgent) Run(
	context.Context, *db.Incident, []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	return &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high",
		PrimaryAction: "run-a", PrimaryReason: "test",
		VerifyAfterSeconds: 1,
	}, nil, nil
}

func (a *countingVerifyAgent) VerifyResolved(context.Context, string, string, *agent.FixSignature) bool {
	a.verifyCalls.Add(1)
	return false
}

func TestVerification_ChecksFromSavedTimers(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.VerifyChecks = 2
		tm.VerifyGapMinutes = 1
	})
	ag := &countingVerifyAgent{lowConfidenceOnceAgent: newLowConfidenceOnceAgent()}
	svc := incident.NewService(
		t.Context(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.DiscardHandler),
	)
	svc.SetUnitForTest(time.Millisecond)

	inc, err := svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "Verify Show",
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, getErr := database.GetIncident(t.Context(), inc.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != db.StatusVerifying && ag.verifyCalls.Load() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("incident never left verifying, status %q", got.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := ag.verifyCalls.Load(); n != 2 {
		t.Fatalf("VerifyResolved calls = %d, want the saved 2", n)
	}
}

type verifyPingAgent struct {
	*escalationAgent

	pings chan struct{}
}

func (a *verifyPingAgent) VerifyResolved(context.Context, string, string, *agent.FixSignature) bool {
	select {
	case a.pings <- struct{}{}:
	default:
	}
	return false
}

func TestApproveEscalation_VerifyAfterFromSavedTimers(t *testing.T) {
	t.Parallel()
	ag := &verifyPingAgent{escalationAgent: newEscalationAgent(), pings: make(chan struct{}, 1)}
	fx := newEscalationFixture(t, ag)
	saveTimers(t, fx.db, func(tm *incident.Timers) {
		tm.ApprovedVerifyMinutes = 1
		tm.FirstVerifyCapMinutes = 5
	})
	fx.svc.SetUnitForTest(time.Second)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)

	if err := fx.svc.ApproveEscalation(t.Context(), inc.ID, fp); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ag.pings:
	case <-time.After(3 * time.Second):
		t.Fatal("first verification check did not follow the saved 1 minute wait")
	}
}

func TestSeedTimeout_FromSavedTimers(t *testing.T) {
	t.Parallel()
	database, inc := newReopenedIncident(t, 0)
	saveTimers(t, database, func(tm *incident.Timers) { tm.StaleRunMinutes = 240 })
	lived := make(chan time.Duration, 1)
	llm := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		start := time.Now()
		<-r.Context().Done()
		lived <- time.Since(start)
	}))
	t.Cleanup(llm.Close)
	cfg := openai.DefaultConfig("test")
	cfg.BaseURL = llm.URL
	jrnl := journal.New(database)
	seed := []openai.ChatCompletionMessage{{Role: "user", Content: "seed"}}
	if err := jrnl.RunStarted(t.Context(), inc.ID, seed); err != nil {
		t.Fatal(err)
	}
	summarizer := agent.NewSummarizer(openai.NewClientWithConfig(cfg), "m")
	svc := incident.NewService(t.Context(), database, jrnl, newBlockingAgent(), nil, summarizer, newSyncNotifier(),
		slog.New(slog.DiscardHandler))
	svc.SetUnitForTest(time.Millisecond)

	if err := svc.Rerun(t.Context(), inc.ID); err != nil {
		t.Fatal(err)
	}

	select {
	case d := <-lived:
		if d < timersSeedAlive {
			t.Fatalf("seed context ended after %v, want the saved 240 minutes (240 ms)", d)
		}
	case <-time.After(timersLate):
		t.Fatal("seed context never ended")
	}
}
