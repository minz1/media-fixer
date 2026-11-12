package incident_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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
	t.Cleanup(func() { stopService(t, svc) })
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
	ctx context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	return gatedExit(ctx, gate, &agent.DiagnosticResult{
		RootCause: "test", Confidence: "high",
		PrimaryAction: "run-a", PrimaryReason: "test",
		VerifyAfterSeconds: 1,
	}, nil, nil)
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
	t.Cleanup(func() { stopService(t, svc) })
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

	if err := fx.svc.ApproveEscalation(
		t.Context(),
		inc.ID,
		fp,
		fx.svc.CurrentVersion(context.Background(), inc.ID),
	); err != nil {
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
	t.Cleanup(func() { stopService(t, svc) })
	svc.SetUnitForTest(time.Millisecond)

	if err := svc.Rerun(t.Context(), inc.ID, svc.CurrentVersion(t.Context(), inc.ID)); err != nil {
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

func newPendingTracker(t *testing.T, database *db.DB) *db.Incident {
	t.Helper()
	inc := &db.Incident{
		Status: statusAwaiting, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Loop Show",
	}
	seedTracked(t, database, inc)
	if err := database.AddReporter(t.Context(), inc.ID, "alice", "discord", "discord-alice"); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{MediaType: "tv", Title: "Loop Show", Season: 1, Episode: 1, StartedAt: time.Now()}
	if err := setTrack(t.Context(), database, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}
	dueNow(t, database, inc.ID)
	return inc
}

func TestBackgroundLoop_ResetsToSavedInterval(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.CheckEveryMinutes = 60
		tm.NoReleaseGraceMinutes = 240
		tm.TrackCapHours = 72
	})
	var checks atomic.Int32
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			checks.Add(1)
			return &agent.PendingOutcomeObservation{}, nil
		},
	}
	svc := incident.NewService(
		t.Context(),
		database,
		nil,
		ag,
		nil,
		nil,
		newSyncNotifier(),
		slog.New(slog.DiscardHandler),
	)
	t.Cleanup(func() { stopService(t, svc) })
	svc.SetUnitForTest(20 * time.Millisecond)
	newPendingTracker(t, database)

	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		svc.BackgroundSweepLoopForTest(ctx)
		close(done)
	}()
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.CheckEveryMinutes = 1
		tm.NoReleaseGraceMinutes = 240
		tm.TrackCapHours = 72
	})

	deadline := time.Now().Add(3 * time.Second)
	for checks.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("checks = %d after 3s, want at least 5 once the saved 1 minute interval applies", checks.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	select {
	case <-done:
	case <-time.After(timersLate):
		t.Fatal("loop did not return after cancel")
	}
}

func newTrackedIncident(t *testing.T, database *db.DB, status db.IncidentStatus, po *db.PendingOutcome) *db.Incident {
	t.Helper()
	inc := &db.Incident{
		Status: status, Source: "discord", ReportedBy: "x", What: "cant_play",
		Title: "Tracked " + strconv.FormatInt(time.Now().UnixNano(), 36),
	}
	seedTracked(t, database, inc)
	if po != nil {
		if err := setTrack(t.Context(), database, inc.ID, po, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return inc
}

func dueAt(t *testing.T, database *db.DB, id string, after time.Duration) bool {
	t.Helper()
	s, err := database.GetIncidentSnapshot(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return !s.NextCheck.IsZero() && !s.NextCheck.After(time.Now().Add(after))
}

func incidentStatus(t *testing.T, database *db.DB, id string) db.IncidentStatus {
	t.Helper()
	got, err := database.GetIncident(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

func queueAgent(obs *agent.PendingOutcomeObservation) *scriptedPendingOutcomeAgent {
	return &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return obs, nil
		},
	}
}

func TestStartPendingOutcomeTracking_FirstCheckFromSavedTimers(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		runResult: &agent.DiagnosticResult{
			RootCause: "missing", Confidence: "high", PrimaryAction: agent.ToolArrSearchMissing, PrimaryReason: "t",
		},
		search: &agent.SearchTarget{MediaType: "movie", Title: "Arrival", Season: -1, Episode: -1},
	}
	svc, database, notif := newPendingOutcomeTestService(t, ag)
	ag.database = database
	saveTimers(t, database, func(tm *incident.Timers) { tm.CheckEveryMinutes = 30 })

	inc, err := svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice", What: "cant_play", Title: "Arrival",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForUserMsg(t, notif)

	if dueAt(t, database, inc.ID, 29*time.Minute) {
		t.Error("next check is due within 29 min, want 30")
	}
	if !dueAt(t, database, inc.ID, 31*time.Minute) {
		t.Error("next check is not due after 31 min, want 30")
	}
}

func TestAdvancePendingOutcome_SavedGrace(t *testing.T) {
	t.Parallel()
	svc, database, _ := newPendingOutcomeTestService(t, queueAgent(&agent.PendingOutcomeObservation{}))
	saveTimers(t, database, func(tm *incident.Timers) { tm.NoReleaseGraceMinutes = 30 })
	for _, tc := range []struct {
		age  time.Duration
		want db.IncidentStatus
	}{{20 * time.Minute, statusAwaiting}, {31 * time.Minute, db.StatusManualTestNeeded}} {
		started := time.Now().Add(-tc.age)
		inc := newTrackedIncident(t, database, statusAwaiting, &db.PendingOutcome{
			MediaType: "movie", Title: "Tracked", Season: -1, Episode: -1, StartedAt: started, LastProgressAt: started,
		})

		svc.CheckNowForTest(t.Context(), inc.ID)

		if got := incidentStatus(t, database, inc.ID); got != tc.want {
			t.Errorf("started %v ago: status %q, want %q", tc.age, got, tc.want)
		}
	}
}

func TestAdvancePendingOutcome_SavedStall(t *testing.T) {
	t.Parallel()
	svc, database, _ := newPendingOutcomeTestService(t,
		queueAgent(&agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: 42}))
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.StallMinutes = 120
		tm.CheckEveryMinutes = 30
	})
	started := time.Now().Add(-90 * time.Minute)
	inc := newTrackedIncident(t, database, statusAwaiting, &db.PendingOutcome{
		MediaType: "movie", Title: "Tracked", Season: -1, Episode: -1,
		StartedAt: started, LastProgressAt: started, LastProgressPct: 42, GrabNotified: true,
	})

	svc.CheckNowForTest(t.Context(), inc.ID)

	if got := incidentStatus(t, database, inc.ID); got != statusAwaiting {
		t.Fatalf("status %q, want verifying (90 min is inside the 120 min stall)", got)
	}
	if dueAt(t, database, inc.ID, 29*time.Minute) || !dueAt(t, database, inc.ID, 31*time.Minute) {
		t.Error("re-arm is not 30 min out")
	}
}

func TestAdvancePendingOutcome_SavedCap(t *testing.T) {
	t.Parallel()
	svc, database, notif := newPendingOutcomeTestService(t,
		queueAgent(&agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: 50}))
	saveTimers(t, database, func(tm *incident.Timers) { tm.TrackCapHours = 2 })
	inc := newTrackedIncident(t, database, statusAwaiting, &db.PendingOutcome{
		MediaType: "movie", Title: "Tracked", Season: -1, Episode: -1,
		StartedAt: time.Now().Add(-3 * time.Hour), LastProgressAt: time.Now(), LastProgressPct: 10, GrabNotified: true,
	})

	svc.CheckNowForTest(t.Context(), inc.ID)

	if got := incidentStatus(t, database, inc.ID); got != db.StatusManualTestNeeded {
		t.Fatalf("status %q, want manual_test_needed", got)
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.msgs) == 0 || !strings.Contains(notif.msgs[len(notif.msgs)-1], "running far longer than expected") {
		t.Errorf("msgs %q, want the cap reason", notif.msgs)
	}
}

func TestKeepSearching_SavedDurationAndText(t *testing.T) {
	t.Parallel()
	svc, database, notif := newPendingOutcomeTestService(t, queueAgent(&agent.PendingOutcomeObservation{}))
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.KeepSearchingDays = 3
		tm.KeepSearchingCheckHours = 2
	})
	started := time.Now().Add(-time.Hour)
	inc := newTrackedIncident(t, database, db.StatusManualTestNeeded, &db.PendingOutcome{
		MediaType: "movie", Title: "Tracked", Season: -1, Episode: -1, StartedAt: started, LastProgressAt: started,
	})

	if err := svc.KeepSearching(t.Context(), inc.ID, svc.CurrentVersion(t.Context(), inc.ID)); err != nil {
		t.Fatal(err)
	}

	po, err := database.GetPendingOutcome(t.Context(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(po.KeepSearchingUntil) - 72*time.Hour; d < -time.Minute || d > time.Minute {
		t.Errorf("KeepSearchingUntil is %v from 3 days out", d)
	}
	if dueAt(t, database, inc.ID, 2*time.Hour-time.Minute) || !dueAt(t, database, inc.ID, 2*time.Hour+time.Minute) {
		t.Error("next check is not 2 h out")
	}

	for days, want := range map[int]string{3: "for up to 3 days", 1: "for up to a day"} {
		saveTimers(t, database, func(tm *incident.Timers) { tm.KeepSearchingDays = days })
		started = time.Now().Add(-time.Hour)
		esc := newTrackedIncident(t, database, statusAwaiting, &db.PendingOutcome{
			MediaType: "movie", Title: "Tracked", Season: -1, Episode: -1, StartedAt: started, LastProgressAt: started,
		})
		svc.CheckNowForTest(t.Context(), esc.ID)
		if got := incidentStatus(t, database, esc.ID); got != db.StatusManualTestNeeded {
			t.Fatalf("days %d: status %q, want manual_test_needed", days, got)
		}
		notif.mu.Lock()
		last := notif.msgs[len(notif.msgs)-1]
		notif.mu.Unlock()
		if !strings.Contains(last, want) {
			t.Errorf("days %d: card %q does not contain %q", days, last, want)
		}
	}
}

func newRetryingService(
	base context.Context, t *testing.T, database *db.DB,
) (*incident.Service, *lowConfidenceOnceAgent, *syncNotifier) {
	t.Helper()
	srv := fakeControlLLMServer(t, `{"verdict":"suggest_alternative","reason":"try again"}`)
	llmCfg := openai.DefaultConfig("test-key")
	llmCfg.BaseURL = srv.URL
	control := agent.NewControlReviewer(openai.NewClientWithConfig(llmCfg), "test-model", slog.New(slog.DiscardHandler))
	ag := newLowConfidenceOnceAgent()
	notif := newSyncNotifier()
	svc := incident.NewService(base, database, nil, ag, control, nil, notif, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopService(t, svc) })
	svc.SetUnitForTest(time.Second)
	return svc, ag, notif
}

func handleRetryShow(t *testing.T, svc *incident.Service, ag *lowConfidenceOnceAgent, title string) *db.Incident {
	t.Helper()
	inc, err := svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice", What: "cant_play", Title: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ag.runCalls:
	case <-time.After(timersLate):
		t.Fatal("first Run never arrived")
	}
	time.Sleep(timersEarly)
	return inc
}

func TestRetryWait_EndsOnCancel(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	saveTimers(t, database, func(tm *incident.Timers) {
		tm.RetryFirstMinutes = 30
		tm.StaleRunMinutes = 41
	})
	base, cancel := context.WithCancel(t.Context())
	svc, ag, _ := newRetryingService(base, t, database)
	inc := handleRetryShow(t, svc, ag, "Cancel Show")

	cancel()

	deadline := time.Now().Add(timersLate)
	for svc.RunningForTest(inc.ID) {
		if time.Now().After(deadline) {
			t.Fatal("run still waiting out its 30 s retry after cancel")
		}
		time.Sleep(timersUnit)
	}
	select {
	case <-ag.runCalls:
		t.Fatal("a cancelled run retried")
	default:
	}
}

func TestTimers_CorruptRowWarnsOncePerBadSpell(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	logs := &syncBuffer{}
	svc := incident.NewService(context.Background(), database, nil, newAlwaysFixedAgent(), nil, nil, newSyncNotifier(),
		slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { stopService(t, svc) })
	corrupt := func() {
		if err := database.SetSetting(t.Context(), incident.TimersKey, "{"); err != nil {
			t.Fatal(err)
		}
	}
	warnings := func() int { return strings.Count(logs.String(), "incident timers unreadable") }

	corrupt()
	svc.ReconcileForTest(t.Context())
	svc.ReconcileForTest(t.Context())
	if n := warnings(); n != 1 {
		t.Fatalf("warnings after two reads of a corrupt row = %d, want 1", n)
	}
	saveTimers(t, database, func(*incident.Timers) {})
	svc.ReconcileForTest(t.Context())
	corrupt()
	svc.ReconcileForTest(t.Context())
	if n := warnings(); n != 2 {
		t.Errorf("warnings after a good read and a new corrupt row = %d, want 2", n)
	}
}
