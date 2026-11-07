package incident_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
)

const staleAge = 20 * time.Minute

func newReopenedIncident(t *testing.T, age time.Duration) (*db.DB, *db.Incident) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reopened.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Left"}
	if err = database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err = database.UpdateIncidentStatus(ctx, inc.ID, db.StatusReopened); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path+"?_time_format=sqlite&_timezone=UTC")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(ctx, `UPDATE incidents SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-age), inc.ID); err != nil {
		t.Fatal(err)
	}
	return database, inc
}

func waitForRun(t *testing.T, ag *alwaysFixedAgent) {
	t.Helper()
	select {
	case <-ag.runCalls:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the reopened incident to be rerun")
	}
}

func TestReconciler_RelaunchesReopenedIncident(t *testing.T) {
	t.Parallel()
	database, _ := newReopenedIncident(t, 0)
	ag := newAlwaysFixedAgent()
	svc := incident.NewService(
		context.Background(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.DiscardHandler),
	)
	t.Cleanup(func() { stopService(t, svc) })

	svc.ReconcileForTest(context.Background())

	waitForRun(t, ag)
}

func TestReconciler_ReopenedWithoutRunRelaunchesQuietly(t *testing.T) {
	t.Parallel()
	for _, age := range []time.Duration{time.Minute, staleAge} {
		database, _ := newReopenedIncident(t, age)
		ag := newAlwaysFixedAgent()
		notif := newSyncNotifier()
		svc := incident.NewService(
			context.Background(),
			database,
			nil,
			ag,
			nil,
			nil,
			notif,
			slog.New(slog.DiscardHandler),
		)
		t.Cleanup(func() { stopService(t, svc) })

		svc.ReconcileForTest(context.Background())

		waitForRun(t, ag)
		notif.mu.Lock()
		if n := countMsgs(notif.msgs, "stopped making progress"); n != 0 {
			t.Errorf("age %v: a reopened incident without a run sent a stale card; msgs %q", age, notif.msgs)
		}
		notif.mu.Unlock()
	}
}

func TestReconciler_PausedRelaunchParks(t *testing.T) {
	t.Parallel()
	database, inc := newReopenedIncident(t, staleAge)
	ctx := context.Background()
	notif := newSyncNotifier()
	discard := slog.New(slog.DiscardHandler)
	svc := incident.NewService(ctx, database, nil, newAlwaysFixedAgent(), nil, nil, notif, discard)
	t.Cleanup(func() { stopService(t, svc) })
	if err := svc.SetAutonomousPaused(ctx, true); err != nil {
		t.Fatal(err)
	}

	svc.ReconcileForTest(ctx)

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if !got.Parked || len(notif.msgs) != 0 {
		t.Errorf("paused relaunch: parked=%v msgs %q", got.Parked, notif.msgs)
	}
}

func TestReconciler_LogsAFailedListing(t *testing.T) {
	t.Parallel()
	database, _ := newReopenedIncident(t, staleAge)
	logs := &syncBuffer{}
	ag := newAlwaysFixedAgent()
	svc := incident.NewService(
		context.Background(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.NewTextHandler(logs, nil)),
	)
	t.Cleanup(func() { stopService(t, svc) })
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	svc.ReconcileForTest(context.Background())

	if !strings.Contains(logs.String(), "reconciler: list driven incidents") {
		t.Errorf("logs = %q, want the failed listing logged", logs.String())
	}
}

func TestSweepStaleRuns_LeavesReopenedIncidentWaitingForTheGlobalSlot(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	notif := newSyncNotifier()
	svc, database, dbPath, _, _ := newRunningIncident(t, logs, notif)
	ctx := context.Background()
	waiting := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Queued",
	}
	if err := database.CreateIncident(ctx, waiting); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rerun(ctx, waiting.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath+"?_time_format=sqlite&_timezone=UTC")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(ctx, `UPDATE incidents SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-staleAge), waiting.ID); err != nil {
		t.Fatal(err)
	}

	svc.ReconcileForTest(ctx)

	if strings.Contains(logs.String(), "recovering stale reopened incident") {
		t.Errorf("a reopened incident with a registered run was rerun; logs:\n%s", logs.String())
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	stale := fmt.Sprintf(
		"⏱️ Incident **Queued** (#%s) stopped making progress for 15 min and is being re-run.", waiting.ID[:8])
	if n := countMsgs(notif.msgs, stale); n != 0 {
		t.Errorf("stale owner DMs = %d, want 0; msgs %q", n, notif.msgs)
	}
}

func TestRerun_HungSummarizerDoesNotHoldTheRunBeforeDiagnosis(t *testing.T) {
	t.Parallel()
	database, inc := newReopenedIncident(t, 0)
	ctx := context.Background()
	hit := make(chan struct{}, 1)
	llm := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hit <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(llm.Close)
	cfg := openai.DefaultConfig("test")
	cfg.BaseURL = llm.URL
	jrnl := journal.New(database)
	seed := []openai.ChatCompletionMessage{{Role: "user", Content: "seed"}}
	if err := jrnl.RunStarted(ctx, inc.ID, seed); err != nil {
		t.Fatal(err)
	}
	ag := newBlockingAgent()
	summarizer := agent.NewSummarizer(openai.NewClientWithConfig(cfg), "m")
	svc := incident.NewService(t.Context(), database, jrnl, ag, nil, summarizer, newSyncNotifier(),
		slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopService(t, svc) })
	svc.SetSeedTimeoutForTest(quietWindow)

	if err := svc.Rerun(ctx, inc.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ag.started:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("a hung summarizer kept the rerun from reaching diagnosis")
	}
	select {
	case <-hit:
	default:
		t.Fatal("the rerun never asked the summarizer, so the seed timeout went untested")
	}
}
