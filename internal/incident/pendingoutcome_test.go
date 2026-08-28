package incident_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

// scriptedPendingOutcomeAgent is a fake AgentRunner whose CheckPendingOutcome
// is driven by a caller-supplied function, so a test can script exactly what
// each successive poll observes (grabbed, stalled, no release, resolved)
// without waiting on real *arr state. Run optionally logs an
// arr_search_missing action (searchLogged) and returns a fixed result, for
// tests that drive the tracking-start path through Service.Handle rather
// than calling AdvancePendingOutcomes directly.
type scriptedPendingOutcomeAgent struct {
	database     *db.DB
	runResult    *agent.DiagnosticResult
	searchParams map[string]any // if non-nil, Run logs this as an arr_search_missing action first
	checkOutcome func(ctx context.Context, po *db.PendingOutcome) (*agent.PendingOutcomeObservation, error)
	runCalls     chan struct{}
}

func (a *scriptedPendingOutcomeAgent) Run(
	ctx context.Context, inc *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	if a.searchParams != nil {
		_ = a.database.LogAction(ctx, &db.ActionLog{
			IncidentID:  inc.ID,
			Action:      agent.ToolArrSearchMissing,
			Params:      a.searchParams,
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
	}
	if a.runCalls != nil {
		a.runCalls <- struct{}{}
	}
	return a.runResult, nil, nil
}

func (a *scriptedPendingOutcomeAgent) VerifyResolved(_ context.Context, _, _ string, _ *agent.FixSignature) bool {
	return true
}
func (a *scriptedPendingOutcomeAgent) ScanRunning(_ context.Context) bool { return false }

func (a *scriptedPendingOutcomeAgent) BuildSummarySeed(
	_ context.Context, _ *db.Incident, _ string,
) []openai.ChatCompletionMessage {
	return nil
}

func (a *scriptedPendingOutcomeAgent) PlanEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *scriptedPendingOutcomeAgent) RunEscalation(_ context.Context, _ *agent.DiagnosticResult) (any, error) {
	return map[string]any{}, nil
}

func (a *scriptedPendingOutcomeAgent) CheckPendingOutcome(
	ctx context.Context, po *db.PendingOutcome,
) (*agent.PendingOutcomeObservation, error) {
	return a.checkOutcome(ctx, po)
}

// newPendingOutcomeTestService builds a Service backed by a real temp-file DB
// (pending-outcome state round-trips through it) and the given fake agent.
func newPendingOutcomeTestService(t *testing.T, ag incident.AgentRunner) (*incident.Service, *db.DB, *syncNotifier) {
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

	notif := newSyncNotifier()
	svc := incident.NewService(context.Background(), database, ag, nil, nil, notif, slog.New(slog.DiscardHandler))
	return svc, database, notif
}

// TestHandleAgentResolved_ArrSearchMissing_StartsPendingOutcomeTracking is the
// end-to-end regression test for routing: a diagnosis whose primary_action is
// arr_search_missing must NOT go through the generic markFixedAndNotify or
// runVerification paths — it must land in StatusVerifying with a
// PendingOutcome recovered from the just-logged action's params, and the
// reporter gets a "searching" DM, not a "fixed" one.
func TestHandleAgentResolved_ArrSearchMissing_StartsPendingOutcomeTracking(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		runResult: &agent.DiagnosticResult{
			RootCause: "missing episode", Confidence: "high",
			PrimaryAction: agent.ToolArrSearchMissing, PrimaryReason: "test",
		},
		searchParams: map[string]any{
			"media_type": "tv", "title": "Rick and Morty", "season": float64(9), "episode": float64(9),
		},
	}
	ag.database = nil // set below once the DB exists
	svc, database, notif := newPendingOutcomeTestService(t, ag)
	ag.database = database
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "Rick and Morty",
	})
	if err != nil {
		t.Fatal(err)
	}

	dm := waitForUserMsg(t, notif)
	if !strings.Contains(dm, "Searching for") {
		t.Fatalf("unexpected DM: %q", dm)
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusVerifying {
		t.Fatalf("status: got %q want %q", got.Status, db.StatusVerifying)
	}

	po, err := database.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if po.Title != "Rick and Morty" || po.Season != 9 || po.Episode != 9 {
		t.Errorf("pending outcome target: got %+v", po)
	}
}

// TestAdvancePendingOutcome_Resolved covers the terminal success path: once
// CheckPendingOutcome reports HasFile, the incident is marked fixed, the
// reporter is notified, and the pending outcome is cleared (not left behind
// for the sweeper to keep polling).
func TestAdvancePendingOutcome_Resolved(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return &agent.PendingOutcomeObservation{HasFile: true}, nil
		},
	}
	svc, database, notif := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Rick and Morty",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := database.AddReporter(ctx, inc.ID, "alice", "discord", "discord-alice"); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9, StartedAt: time.Now()}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc.AdvancePendingOutcomes(ctx)

	dm := waitForUserMsg(t, notif)
	if !strings.Contains(dm, "fixed automatically") {
		t.Fatalf("unexpected DM: %q", dm)
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusAgentFixed {
		t.Errorf("status: got %q want %q", got.Status, db.StatusAgentFixed)
	}
	if _, getErr := database.GetPendingOutcome(ctx, inc.ID); getErr == nil {
		t.Error("expected pending outcome to be cleared once resolved")
	}
}

// TestAdvancePendingOutcome_GrabbedNotifiesOnceThenTracksProgress covers the
// "found it, downloading" milestone: the reporter is notified the first time
// a queue item appears, but not again on a later poll that just shows more
// progress.
func TestAdvancePendingOutcome_GrabbedNotifiesOnceThenTracksProgress(t *testing.T) {
	t.Parallel()
	var pct atomic.Int64
	pct.Store(10)
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return &agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: float64(pct.Load())}, nil
		},
	}
	svc, database, notif := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Rick and Morty",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := database.AddReporter(ctx, inc.ID, "alice", "discord", "discord-alice"); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{
		MediaType:      "tv",
		Title:          "Rick and Morty",
		Season:         9,
		Episode:        9,
		StartedAt:      time.Now(),
		LastProgressAt: time.Now(),
	}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc.AdvancePendingOutcomes(ctx)
	dm := waitForUserMsg(t, notif)
	if !strings.Contains(dm, "Found") || !strings.Contains(dm, "downloading") {
		t.Fatalf("unexpected first-poll DM: %q", dm)
	}

	// Second poll: more progress, same stage — must not send a second DM.
	// Re-fetch first: advanceDownloading already persisted GrabNotified=true
	// after the first poll, and re-saving the original local po here (which
	// never saw that mutation) would silently reset it to false, defeating
	// the very thing this test checks.
	po, err := database.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	pct.Store(50)
	if setErr := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); setErr != nil {
		t.Fatal(setErr)
	}
	svc.AdvancePendingOutcomes(ctx)
	select {
	case second := <-notif.userMsgs:
		t.Fatalf("received a second DM on the same download: %q", second)
	case <-time.After(300 * time.Millisecond):
	}

	got, err := database.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GrabNotified || got.LastProgressPct != 50 {
		t.Errorf("got %+v", got)
	}
}

// TestAdvancePendingOutcome_StalledDownload_Escalates covers the stall
// timeout: progress that stopped moving well past pendingOutcomeStallTimeout
// must escalate to the owner rather than track forever.
func TestAdvancePendingOutcome_StalledDownload_Escalates(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			// Same progress every time — never advances, matching what a
			// real stalled download's queue entry would report.
			return &agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: 42}, nil
		},
	}
	svc, database, notif := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Backrooms",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	// LastProgressAt well past the stall timeout, and already GrabNotified
	// (this isn't the first sighting) so we isolate the escalation path from
	// the grab-notification path.
	staleProgress := time.Now().Add(-2 * time.Hour)
	po := &db.PendingOutcome{
		MediaType: "movie", Title: "Backrooms",
		Season: -1, Episode: -1,
		StartedAt: staleProgress, LastProgressAt: staleProgress, LastProgressPct: 42,
		GrabNotified: true,
	}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc.AdvancePendingOutcomes(ctx)

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusManualTestNeeded {
		t.Fatalf("status: got %q want %q", got.Status, db.StatusManualTestNeeded)
	}
	if len(notif.msgs) == 0 || !strings.Contains(notif.msgs[len(notif.msgs)-1], "stalled") {
		t.Errorf("expected an owner notification mentioning the stall, got %v", notif.msgs)
	}
}

// TestAdvancePendingOutcome_NoReleaseFound_EscalatesAfterGrace covers the
// "nothing ever showed up in the queue" path: past the grace period with no
// queue item and no keep-searching opt-in, the incident escalates.
func TestAdvancePendingOutcome_NoReleaseFound_EscalatesAfterGrace(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return &agent.PendingOutcomeObservation{}, nil // nothing found, nothing in queue
		},
	}
	svc, database, notif := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "New Show",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	longAgo := time.Now().Add(-30 * time.Minute) // past pendingOutcomeGracePeriod (15m)
	po := &db.PendingOutcome{
		MediaType:      "tv",
		Title:          "New Show",
		Season:         1,
		Episode:        1,
		StartedAt:      longAgo,
		LastProgressAt: longAgo,
	}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc.AdvancePendingOutcomes(ctx)

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusManualTestNeeded {
		t.Fatalf("status: got %q want %q", got.Status, db.StatusManualTestNeeded)
	}
	if len(notif.msgs) == 0 || !strings.Contains(notif.msgs[len(notif.msgs)-1], "no release was found") {
		t.Errorf("expected an owner notification mentioning no release found, got %v", notif.msgs)
	}

	// The pending outcome must survive escalation (not be cleared) so
	// KeepSearching has something to re-arm.
	if _, getErr := database.GetPendingOutcome(ctx, inc.ID); getErr != nil {
		t.Errorf("expected pending outcome to survive escalation for KeepSearching, got error: %v", getErr)
	}
}

// TestKeepSearching_ReArmsAndResumesSweeping is the end-to-end regression
// test for the "brand new content" case: after a no-release escalation, an
// owner approving "Keep searching" must move the incident back into
// StatusVerifying so the sweeper resumes checking it, and a subsequent
// AdvancePendingOutcomes call (still observing no queue item) must NOT
// escalate again immediately — it's within the keep-searching window now.
func TestKeepSearching_ReArmsAndResumesSweeping(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return &agent.PendingOutcomeObservation{}, nil
		},
	}
	svc, database, _ := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusManualTestNeeded,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "New Show",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{
		MediaType: "tv",
		Title:     "New Show",
		Season:    1,
		Episode:   1,
		StartedAt: time.Now().Add(-time.Hour),
	}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := svc.KeepSearching(ctx, inc.ID); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusVerifying {
		t.Fatalf("status after KeepSearching: got %q want %q", got.Status, db.StatusVerifying)
	}

	svc.AdvancePendingOutcomes(ctx)

	got, err = database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusVerifying {
		t.Fatalf("expected KeepSearching to prevent immediate re-escalation, status: got %q", got.Status)
	}
	afterPo, err := database.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !afterPo.KeepSearching {
		t.Error("expected keep_searching to remain set")
	}
}

// TestKeepSearching_RequiresManualTestNeeded confirms KeepSearching refuses
// to act on an incident that isn't actually awaiting that decision (e.g.
// double-submitting the button, or an incident that was independently
// rerun in the meantime).
func TestKeepSearching_RequiresManualTestNeeded(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{}
	svc, database, _ := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "New Show"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{MediaType: "tv", Title: "New Show", Season: 1, Episode: 1, StartedAt: time.Now()}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := svc.KeepSearching(ctx, inc.ID); err == nil {
		t.Error("expected an error keep-searching an incident not in manual_test_needed")
	}
}
