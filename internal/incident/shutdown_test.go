package incident_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

const stopServiceTimeout = 5 * time.Second

func stopService(t *testing.T, svc *incident.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stopServiceTimeout)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Errorf("service shutdown: %v", err)
	}
}

type drainingAgent struct {
	*escalationAgent

	db       *db.DB
	started  chan string
	release  chan struct{}
	finished atomic.Bool
	mu       sync.Mutex
	seeds    [][]openai.ChatCompletionMessage
}

func newDrainingAgent(d *db.DB) *drainingAgent {
	return &drainingAgent{
		escalationAgent: newEscalationAgent(), db: d, started: make(chan string, 4),
	}
}

func (a *drainingAgent) Run(
	ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage, _ agent.Gate,
) agent.RunExit {
	a.mu.Lock()
	a.seeds = append(a.seeds, seed)
	a.mu.Unlock()
	a.started <- inc.ID
	if a.release != nil {
		<-a.release
	} else {
		<-ctx.Done()
	}
	time.Sleep(50 * time.Millisecond)
	if err := a.db.SetAutonomousLocked(context.WithoutCancel(ctx), inc.ID, false); err == nil {
		a.finished.Store(true)
	}
	return exitOf(nil, nil, context.Canceled)
}

func waitStarted(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the run to start")
	}
	return ""
}

func report(t *testing.T, svc *incident.Service, title string) *db.Incident {
	t.Helper()
	inc, err := svc.Handle(context.Background(), &incident.Report{
		Source: "discord", ReportedBy: "alice", What: "cant_play", Title: title,
	})
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func TestA10_10_ShutdownWaitsForRunsBeforeDBClose(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	ag := newDrainingAgent(database)
	svc := incident.NewService(
		context.Background(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.DiscardHandler))
	report(t, svc, "Halt and Catch Fire")
	waitStarted(t, ag.started)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !ag.finished.Load() {
		t.Error("Shutdown returned before the run finished its last write")
	}
}

func TestA10_10_ShutdownBoundNamesStragglers(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	ag := newDrainingAgent(database)
	ag.release = make(chan struct{})
	svc := incident.NewService(
		context.Background(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.DiscardHandler))
	inc := report(t, svc, "Halt and Catch Fire")
	waitStarted(t, ag.started)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := svc.Shutdown(ctx)
	if !errors.Is(err, incident.ErrShutdownTimeout) || !strings.Contains(err.Error(), "run:"+inc.ShortID()) {
		t.Errorf("Shutdown = %v, want ErrShutdownTimeout naming run:%s", err, inc.ShortID())
	}
	close(ag.release)
	stopService(t, svc)
}

func TestA10_10_ReportDuringShutdownIsDrivenAfterRestart(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	ag := newDrainingAgent(database)
	notif := newSyncNotifier()
	first := incident.NewService(
		context.Background(),
		database,
		nil,
		ag,
		nil,
		nil,
		notif,
		slog.New(slog.DiscardHandler),
	)
	stopService(t, first)

	inc := report(t, first, "Halt and Catch Fire")
	got, err := database.GetIncident(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusOpen {
		t.Fatalf("status after a report during shutdown = %s, want open", got.Status)
	}
	select {
	case <-ag.started:
		t.Fatal("a report during shutdown started a run")
	case <-time.After(50 * time.Millisecond):
	}

	second := incident.NewService(
		context.Background(),
		database,
		nil,
		ag,
		nil,
		nil,
		notif,
		slog.New(slog.DiscardHandler),
	)
	t.Cleanup(func() { stopService(t, second) })
	second.StartRecovery()
	if id := waitStarted(t, ag.started); id != inc.ID {
		t.Fatalf("recovery ran %s, want %s", id, inc.ID)
	}
	ag.mu.Lock()
	seed := ag.seeds[0]
	ag.mu.Unlock()
	if seed != nil {
		t.Errorf("recovered open incident ran with seed %v, want a first run", seed)
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if n := countMsgs(notif.msgs, "🔁 Taking another look at **Halt and Catch Fire**."); n != 0 {
		t.Errorf("recovery sent %d taking-another-look notes, want none", n)
	}
}

func TestBackgroundLoop_LockEpisodeTickRearmsAlert(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	notif := newSyncNotifier()
	svc := incident.NewService(context.Background(), database, nil, nil, nil, nil, notif, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopService(t, svc) })
	svc.SetUnitForTest(time.Millisecond)
	var first []*db.Incident
	for _, title := range []string{"A", "B", "C", "D", "E", "F"} {
		first = append(first, report(t, svc, title))
	}
	for _, inc := range first[:2] {
		if err := database.UpdateIncidentStatus(context.Background(), inc.ID, db.StatusResolved); err != nil {
			t.Fatal(err)
		}
	}
	if svc.LockAlertArmedForTest() {
		t.Fatal("the lock alert never fired for six open incidents")
	}
	svc.Start()
	deadline := time.Now().Add(5 * time.Second)
	for !svc.LockAlertArmedForTest() {
		if time.Now().After(deadline) {
			t.Fatal("the loop's tick never re-armed the lock alert")
		}
		time.Sleep(time.Millisecond)
	}
	for _, inc := range first[:2] {
		if err := database.UpdateIncidentStatus(context.Background(), inc.ID, db.StatusReopened); err != nil {
			t.Fatal(err)
		}
	}
	report(t, svc, "G")
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if n := containsCount(notif.msgs, "possible systemic failure"); n != 2 {
		t.Errorf("lock alerts = %d, want 2 after the loop's tick re-armed the alert; msgs %q", n, notif.msgs)
	}
}

func TestStart_LogsSweepLoopStarted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	svc := incident.NewService(context.Background(), openTimersDB(t), nil, nil, nil, nil, newSyncNotifier(), log)
	t.Cleanup(func() { stopService(t, svc) })
	mu.Lock()
	before := buf.String()
	mu.Unlock()
	if before != "" {
		t.Fatalf("NewService logged %q, want nothing before Start", before)
	}
	svc.Start()
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), `level=INFO msg="incident sweep loop started"`) {
		t.Errorf("Start logged %q, want the sweep loop started line at INFO", buf.String())
	}
}

func TestRunManager_SupersedeAfterShutdownIsCancelled(t *testing.T) {
	t.Parallel()
	svc := incident.NewService(
		context.Background(), openTimersDB(t), nil, nil, nil, nil, newSyncNotifier(), slog.New(slog.DiscardHandler))
	stopService(t, svc)
	if ctx := svc.SupersedeForTest("late"); ctx.Err() == nil {
		t.Error("a run registered after Shutdown got a live context")
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func containsCount(msgs []string, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

func openIncidents(t *testing.T, d *db.DB, titles ...string) []*db.Incident {
	t.Helper()
	var out []*db.Incident
	for _, title := range titles {
		inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: title}
		if err := d.CreateIncident(context.Background(), inc); err != nil {
			t.Fatal(err)
		}
		out = append(out, inc)
	}
	return out
}

func TestA10_10_RecoveryLeavesOpenIncidentsWhilePaused(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	openIncidents(t, database, "A", "B")
	ag := newDrainingAgent(database)
	logs := &syncBuffer{}
	svc := incident.NewService(
		context.Background(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.NewTextHandler(logs, nil)))
	if err := svc.SetAutonomousPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	svc.RecoverZombies(context.Background())
	stopService(t, svc)
	out := logs.String()
	if n := strings.Count(out, "starting open incident left without a run"); n != 0 {
		t.Errorf("recovery started %d open incidents while paused", n)
	}
	if n := strings.Count(out, "level=WARN"); n != 0 {
		t.Errorf("recovery while paused logged %d WARN lines:\n%s", n, out)
	}
	if n := strings.Count(out, "level=INFO msg=\"open incident recovery skipped"); n != 1 {
		t.Errorf("recovery while paused logged %d skip lines, want 1:\n%s", n, out)
	}
}

func TestA10_10_RecoveryLeavesLockedOpenIncident(t *testing.T) {
	t.Parallel()
	database := openTimersDB(t)
	incs := openIncidents(t, database, "Locked", "Free")
	if err := database.SetAutonomousLocked(context.Background(), incs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	ag := newDrainingAgent(database)
	logs := &syncBuffer{}
	svc := incident.NewService(
		context.Background(), database, nil, ag, nil, nil, newSyncNotifier(), slog.New(slog.NewTextHandler(logs, nil)))
	svc.RecoverZombies(context.Background())
	if id := waitStarted(t, ag.started); id != incs[1].ID {
		t.Errorf("recovery ran %s, want the unlocked %s", id, incs[1].ID)
	}
	stopService(t, svc)
	out := logs.String()
	if strings.Contains(out, "incident="+incs[0].ID) {
		t.Errorf("recovery touched the locked incident:\n%s", out)
	}
}
