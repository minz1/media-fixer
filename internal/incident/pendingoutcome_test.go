package incident_test

import (
	"context"
	"errors"
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

func (a *scriptedPendingOutcomeAgent) ExecuteApprovedPlan(
	_ context.Context, _ *agent.DiagnosticResult, _ []byte,
) (any, error) {
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
	svc := incident.NewService(context.Background(), database, nil, ag, nil, nil, notif, slog.New(slog.DiscardHandler))
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

func TestAdvancePendingOutcome_QueueItemGoneClearsDownloadingStage(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return &agent.PendingOutcomeObservation{}, nil
		},
	}
	svc, database, _ := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusVerifying, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "New Show",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	po := &db.PendingOutcome{
		MediaType: "tv", Title: "New Show", Season: 1, Episode: 1,
		StartedAt: now, LastStage: "downloading", LastProgressPct: 37, LastProgressAt: now,
	}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, now); err != nil {
		t.Fatal(err)
	}

	svc.AdvancePendingOutcomes(ctx)

	got, err := database.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStage == "downloading" {
		t.Error("stage still downloading after the queue item vanished; the next progress note would claim a download")
	}
}

// TestAdvancePendingOutcome_CheckErrors_RescheduledNotEscalated is the
// end-to-end regression test for the false-escalation bug CheckPendingOutcome
// itself was fixed for (see internal/agent's
// TestCheckPendingOutcome_QueueFetchFails_PropagatesError): a transient
// CheckPendingOutcome error landing on exactly the poll where the grace
// period has elapsed must reschedule and retry — the same as any other
// transient failure — never escalate with "no release was found" just
// because the failure happened to look like an empty observation.
func TestAdvancePendingOutcome_CheckErrors_RescheduledNotEscalated(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return nil, errors.New("transient sonarr outage")
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
	// Past pendingOutcomeGracePeriod (15m) — exactly the condition under
	// which the pre-fix code would have escalated on a degraded observation.
	longAgo := time.Now().Add(-30 * time.Minute)
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
	if got.Status != db.StatusVerifying {
		t.Errorf("status: got %q, want %q (a check error must not escalate)", got.Status, db.StatusVerifying)
	}
	if len(notif.msgs) != 0 {
		t.Errorf("expected no owner notification on a check error, got %v", notif.msgs)
	}
	if _, getErr := database.GetPendingOutcome(ctx, inc.ID); getErr != nil {
		t.Errorf("expected the pending outcome to still be scheduled for retry, got error: %v", getErr)
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

// TestAdvancePendingOutcome_PersistentCheckFailureEventuallyEscalates pins the
// bound on the error path. Propagating a queue-fetch error (rather than
// degrading it into "no release found") is correct, but the error branch
// returned before the stall and overall-cap checks in advanceDownloading /
// advanceNoQueueItem ever ran — so a persistent failure (a rotated Sonarr API
// key, Sonarr moved) left the incident re-polling every five minutes forever,
// escalating never, with the reporters' last message still "downloading now".
func TestAdvancePendingOutcome_PersistentCheckFailureEventuallyEscalates(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return nil, errors.New("sonarr: 401 unauthorized")
		},
	}
	svc, database, notif := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Lanterns",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	// Well past the absolute give-up point for one tracking attempt.
	po := &db.PendingOutcome{
		MediaType: "tv", Title: "Lanterns", Season: 1, Episode: 3,
		StartedAt: time.Now().Add(-24 * time.Hour),
	}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc.AdvancePendingOutcomes(ctx)

	notif.mu.Lock()
	msgs := strings.Join(notif.msgs, "\n")
	notif.mu.Unlock()
	if !strings.Contains(msgs, "Lanterns") {
		t.Errorf("owner was never told the pending outcome gave up; messages: %q", msgs)
	}
	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusManualTestNeeded {
		t.Errorf("status = %q, want manual_test_needed: a pending outcome whose state can "+
			"no longer be checked must reach a human, not retry forever", got.Status)
	}
}

// TestAdvancePendingOutcome_TransientCheckFailureStillRetries is the other
// half of the bound above: a failure inside the cap must NOT escalate, or a
// blip in the *arr API would page the owner on every sweep.
func TestAdvancePendingOutcome_TransientCheckFailureStillRetries(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			return nil, errors.New("sonarr: connection refused")
		},
	}
	svc, database, _ := newPendingOutcomeTestService(t, ag)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusVerifying, Source: "discord", ReportedBy: "x",
		What: "cant_play", Title: "Lanterns",
	}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{
		MediaType: "tv", Title: "Lanterns", Season: 1, Episode: 3,
		StartedAt: time.Now().Add(-2 * time.Minute),
	}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}

	svc.AdvancePendingOutcomes(ctx)

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusVerifying {
		t.Errorf("status = %q, want verifying: a transient check failure well inside the "+
			"cap must keep retrying rather than escalate", got.Status)
	}
}

const keepSearchingTolerance = time.Minute

type keepSearchingFixture struct {
	svc      *incident.Service
	database *db.DB
	notif    *syncNotifier
	inc      *db.Incident
	obs      atomic.Pointer[agent.PendingOutcomeObservation]
	checkErr atomic.Bool
}

func newKeepSearchingFixture(t *testing.T, status db.IncidentStatus, po *db.PendingOutcome) *keepSearchingFixture {
	t.Helper()
	f := &keepSearchingFixture{}
	f.obs.Store(&agent.PendingOutcomeObservation{})
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			if f.checkErr.Load() {
				return nil, errors.New("arr unreachable")
			}
			return f.obs.Load(), nil
		},
	}
	f.svc, f.database, f.notif = newPendingOutcomeTestService(t, ag)
	f.inc = newTrackedIncident(t, f.database, status, po)
	if err := f.database.AddReporter(t.Context(), f.inc.ID, "alice", "discord", "discord-alice"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *keepSearchingFixture) track(t *testing.T) *db.PendingOutcome {
	t.Helper()
	po, err := f.database.GetPendingOutcome(t.Context(), f.inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return po
}

func (f *keepSearchingFixture) ageTrack(t *testing.T, d time.Duration) {
	t.Helper()
	po := f.track(t)
	po.StartedAt = po.StartedAt.Add(-d)
	po.LastProgressAt = po.LastProgressAt.Add(-d)
	if err := f.database.SetPendingOutcome(t.Context(), f.inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func keepSearchingTrack(startedAgo time.Duration) *db.PendingOutcome {
	return &db.PendingOutcome{
		MediaType:          "tv",
		Title:              "Tracked",
		Season:             1,
		Episode:            1,
		StartedAt:          time.Now().Add(-startedAgo),
		LastProgressAt:     time.Now().Add(-startedAgo),
		KeepSearching:      true,
		KeepSearchingUntil: time.Now().Add(24 * time.Hour),
	}
}

func assertNear(t *testing.T, name string, got time.Time) {
	t.Helper()
	if d := time.Since(got); d < -keepSearchingTolerance || d > keepSearchingTolerance {
		t.Errorf("%s = %v, want within %v of now", name, got, keepSearchingTolerance)
	}
}

func TestKeepSearching_ResetsTrack(t *testing.T) {
	t.Parallel()
	po := &db.PendingOutcome{
		MediaType: "tv", Title: "Tracked", Season: 1, Episode: 1,
		StartedAt:       time.Now().Add(-48 * time.Hour),
		LastProgressAt:  time.Now().Add(-24 * time.Hour),
		LastProgressPct: 80,
		LastStage:       "downloading",
		GrabNotified:    true,
	}
	f := newKeepSearchingFixture(t, db.StatusManualTestNeeded, po)

	if err := f.svc.KeepSearching(t.Context(), f.inc.ID); err != nil {
		t.Fatal(err)
	}

	got := f.track(t)
	assertNear(t, "StartedAt", got.StartedAt)
	assertNear(t, "LastProgressAt", got.LastProgressAt)
	if got.LastProgressPct != 0 || got.LastStage != "searching" || got.GrabNotified {
		t.Errorf("track not reset: pct %v stage %q grabNotified %v",
			got.LastProgressPct, got.LastStage, got.GrabNotified)
	}
}

func TestKeepSearching_NoDownloadingProgressNote(t *testing.T) {
	t.Parallel()
	po := &db.PendingOutcome{
		MediaType: "tv", Title: "Tracked", Season: 1, Episode: 1,
		StartedAt:       time.Now().Add(-48 * time.Hour),
		LastProgressAt:  time.Now().Add(time.Minute),
		LastProgressPct: 80,
		LastStage:       "downloading",
		GrabNotified:    true,
	}
	f := newKeepSearchingFixture(t, db.StatusManualTestNeeded, po)

	if err := f.svc.KeepSearching(t.Context(), f.inc.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.AdvanceReporterProgress(t.Context(), time.Now().Add(61*time.Minute))

	f.notif.mu.Lock()
	defer f.notif.mu.Unlock()
	for _, m := range f.notif.msgs {
		if strings.Contains(m, "downloading") {
			t.Errorf("reporter told about a download that is not running: %q", m)
		}
	}
}

func TestKeepSearching_LateReleaseGetsFullBudget(t *testing.T) {
	t.Parallel()
	f := newKeepSearchingFixture(t, db.StatusVerifying, keepSearchingTrack(72*time.Hour))
	f.obs.Store(&agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: 5})

	f.svc.AdvancePendingOutcomes(t.Context())

	if got := incidentStatus(t, f.database, f.inc.ID); got != db.StatusVerifying {
		t.Fatalf("status = %q, want verifying: a late release must not escalate on first sighting", got)
	}
	if dm := waitForUserMsg(t, f.notif); !strings.Contains(dm, "Found") {
		t.Errorf("reporter DM = %q, want the Found DM", dm)
	}
	got := f.track(t)
	assertNear(t, "StartedAt", got.StartedAt)
	assertNear(t, "LastProgressAt", got.LastProgressAt)
}

func TestKeepSearching_CheckErrorMeasuredFromLastCheck(t *testing.T) {
	t.Parallel()
	f := newKeepSearchingFixture(t, db.StatusVerifying, keepSearchingTrack(13*time.Hour))

	f.svc.AdvancePendingOutcomes(t.Context())
	assertNear(t, "StartedAt after an empty check", f.track(t).StartedAt)

	f.ageTrack(t, time.Hour)
	f.checkErr.Store(true)
	f.svc.AdvancePendingOutcomes(t.Context())

	if got := incidentStatus(t, f.database, f.inc.ID); got != db.StatusVerifying {
		t.Errorf("status = %q, want verifying: the cap counts from the last successful check", got)
	}
}

func TestKeepSearching_CheckErrorAllowsOneCheckIntervalPastCap(t *testing.T) {
	t.Parallel()
	f := newKeepSearchingFixture(t, db.StatusVerifying, keepSearchingTrack(0))
	tm := incident.DefaultTimers()
	tm.Set("track_cap_hours", 1)
	tm.Set("stall_minutes", 59)
	tm.Set("keep_searching_check_hours", 2)
	if err := incident.SaveTimers(t.Context(), f.database, tm); err != nil {
		t.Fatal(err)
	}

	f.svc.AdvancePendingOutcomes(t.Context())
	f.ageTrack(t, 2*time.Hour)
	f.checkErr.Store(true)
	f.svc.AdvancePendingOutcomes(t.Context())
	if got := incidentStatus(t, f.database, f.inc.ID); got != db.StatusVerifying {
		t.Fatalf("status = %q, want verifying: a failed check at the scheduled time is inside check + cap", got)
	}

	f.ageTrack(t, time.Hour+time.Minute)
	f.svc.AdvancePendingOutcomes(t.Context())
	if got := incidentStatus(t, f.database, f.inc.ID); got == db.StatusVerifying {
		t.Errorf("status = %q, want escalated past check + cap", got)
	}
}

func TestKeepSearching_SecondDownloadGetsFreshBudget(t *testing.T) {
	t.Parallel()
	f := newKeepSearchingFixture(t, db.StatusVerifying, keepSearchingTrack(time.Minute))

	f.obs.Store(&agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: 80})
	f.svc.AdvancePendingOutcomes(t.Context())
	if dm := waitForUserMsg(t, f.notif); !strings.Contains(dm, "Found") {
		t.Fatalf("first DM = %q, want Found", dm)
	}

	f.obs.Store(&agent.PendingOutcomeObservation{})
	f.ageTrack(t, 0)
	f.svc.AdvancePendingOutcomes(t.Context())
	if pct := f.track(t).LastProgressPct; pct != 0 {
		t.Fatalf("LastProgressPct after an empty check = %v, want 0", pct)
	}
	f.ageTrack(t, 2*time.Hour)

	f.obs.Store(&agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: 5})
	f.svc.AdvancePendingOutcomes(t.Context())

	if got := incidentStatus(t, f.database, f.inc.ID); got != db.StatusVerifying {
		t.Fatalf("status = %q, want verifying: the second download is a new budget", got)
	}
	if dm := waitForUserMsg(t, f.notif); !strings.Contains(dm, "Found") {
		t.Errorf("second DM = %q, want a second Found DM", dm)
	}
	assertNear(t, "LastProgressAt", f.track(t).LastProgressAt)
}
