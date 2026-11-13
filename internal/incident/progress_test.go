package incident_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func action(name string, status db.ActionStatus, at time.Time) *db.ActionLog {
	return &db.ActionLog{Action: name, Status: status, AppliedAt: &at}
}

func TestProgressPhrases(t *testing.T) {
	t.Parallel()
	since := time.Now()
	after := since.Add(time.Minute)
	actions := []*db.ActionLog{
		action("sonarr_rescan", db.ActionApplied, since.Add(-time.Minute)),
		action("refresh_decypharr_links", db.ActionApplied, after),
		action("restart_decypharr", db.ActionApplied, after),
		action("sonarr_rescan", db.ActionFailed, after),
		action("clear_jellyfin_cache", db.ActionApplied, after),
	}

	want := "fixing the media storage|reloading the app"
	if got := strings.Join(incident.ProgressPhrasesForTest(actions, since, nil), "|"); got != want {
		t.Errorf("phrases = %q, want %q", got, want)
	}

	searching := &db.PendingOutcome{LastStage: "searching"}
	if got := strings.Join(incident.ProgressPhrasesForTest(actions, since, searching), "|"); got != want {
		t.Errorf("phrases while searching = %q, want %q", got, want)
	}

	stale := &db.PendingOutcome{LastStage: "downloading", LastProgressPct: 37, LastProgressAt: since.Add(-time.Hour)}
	if got := strings.Join(incident.ProgressPhrasesForTest(actions, since, stale), "|"); got != want {
		t.Errorf("phrases with stalled download = %q, want %q", got, want)
	}

	failed := &db.PendingOutcome{LastStage: "failed", LastProgressPct: 12, LastProgressAt: after}
	if got := strings.Join(incident.ProgressPhrasesForTest(actions, since, failed), "|"); got != want {
		t.Errorf("phrases with failed download = %q, want %q", got, want)
	}

	po := &db.PendingOutcome{LastStage: "downloading", LastProgressPct: 43.4, LastProgressAt: after}
	want += "|downloading a new copy (43%)"
	if got := strings.Join(incident.ProgressPhrasesForTest(actions, since, po), "|"); got != want {
		t.Errorf("phrases with download = %q, want %q", got, want)
	}
}

func newProgressIncident(
	t *testing.T,
	status db.IncidentStatus,
) (*incident.Service, *db.DB, *db.Incident, *captureNotifier) {
	t.Helper()
	svc, database, notif := newTestService(t)
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "fixture", What: "cant_play", Title: "X"}
	lifecycletest.Incident(t, database, inc)
	if err := database.AddReporter(ctx, inc.ID, "alice", "discord", "discord-alice"); err != nil {
		t.Fatal(err)
	}
	lifecycletest.MoveTo(t, database, inc.ID, status)
	return svc, database, inc, notif
}

func logApplied(t *testing.T, database *db.DB, incID, name string) {
	t.Helper()
	err := database.LogAction(context.Background(), &db.ActionLog{
		IncidentID: incID, Action: name, TriggeredBy: "agent", Status: db.ActionApplied,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func logAppliedAt(t *testing.T, database *db.DB, incID, name string, at time.Time) {
	t.Helper()
	err := database.LogAction(context.Background(), &db.ActionLog{
		IncidentID: incID, Action: name, TriggeredBy: "agent", Status: db.ActionApplied, AppliedAt: &at,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestF7_NothingNew(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	ctx := context.Background()

	svc.ArmReporterNoteForTest(ctx, inc.ID, time.Now())

	if len(notif.msgs) != 0 {
		t.Errorf("msgs = %q, want none", notif.msgs)
	}
	last, err := database.LastReporterNoteAt(ctx, inc.ID)
	if err != nil || !last.IsZero() {
		t.Errorf("last_reporter_note_at = %v, %v; want unset", last, err)
	}
}

func TestF7_HourlyCap(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	ctx := context.Background()

	logApplied(t, database, inc.ID, "refresh_decypharr_links")
	start := time.Now()
	svc.ArmReporterNoteForTest(ctx, inc.ID, start)
	logApplied(t, database, inc.ID, "sonarr_rescan")
	svc.ArmReporterNoteForTest(ctx, inc.ID, start.Add(30*time.Minute))

	want := []string{"⏳ Still working on **X**: fixing the media storage."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif.msgs, want)
	}

	svc.ArmReporterNoteForTest(ctx, inc.ID, start.Add(61*time.Minute))

	want = append(want, "⏳ Still working on **X**: updating the library.")
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Errorf("msgs = %q, want %q", notif.msgs, want)
	}
}

func TestF7_OnlyActiveStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []db.IncidentStatus{db.StatusManualTestNeeded, db.StatusResolved} {
		svc, database, inc, notif := newProgressIncident(t, status)
		logApplied(t, database, inc.ID, "restart_jellyfin")

		svc.ArmReporterNoteForTest(context.Background(), inc.ID, time.Now())

		if len(notif.msgs) != 0 {
			t.Errorf("%s: msgs = %q, want none", status, notif.msgs)
		}
	}
}

func TestF7_SavedGap(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	now := time.Now()
	if _, err := database.ClaimReporterNote(ctx, inc.ID, now.Add(-20*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	logApplied(t, database, inc.ID, "refresh_decypharr_links")

	svc.ArmReporterNoteForTest(ctx, inc.ID, now)

	want := []string{"⏳ Still working on **X**: fixing the media storage."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Errorf("msgs = %q, want %q", notif.msgs, want)
	}
}

func TestE6_ActionAfterGapSendsNow(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	if _, err := database.ClaimReporterNote(ctx, inc.ID, base, 0); err != nil {
		t.Fatal(err)
	}
	logAppliedAt(t, database, inc.ID, "refresh_decypharr_links", base.Add(20*time.Minute))

	svc.ArmReporterNoteForTest(ctx, inc.ID, base.Add(21*time.Minute))

	want := []string{"⏳ Still working on **X**: fixing the media storage."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif.msgs, want)
	}
}

func TestE6_TwoActionsInsideGapGiveOneNote(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	base := time.Now()
	if _, err := database.ClaimReporterNote(ctx, inc.ID, base, 0); err != nil {
		t.Fatal(err)
	}
	logAppliedAt(t, database, inc.ID, "refresh_decypharr_links", base.Add(time.Minute))
	svc.ArmReporterNoteForTest(ctx, inc.ID, base.Add(time.Minute))
	if len(notif.msgs) != 0 {
		t.Fatalf("inside the gap sent %q, want none yet", notif.msgs)
	}
	logAppliedAt(t, database, inc.ID, "sonarr_rescan", base.Add(2*time.Minute))
	svc.ArmReporterNoteForTest(ctx, inc.ID, base.Add(2*time.Minute))
	if len(notif.msgs) != 0 {
		t.Fatalf("inside the gap sent %q, want none yet", notif.msgs)
	}

	svc.FireProgressTimerForTest(ctx, inc.ID, base.Add(15*time.Minute))

	want := []string{"⏳ Still working on **X**: fixing the media storage, updating the library."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif.msgs, want)
	}
}

func TestE6_NoActionNoNote(t *testing.T) {
	t.Parallel()
	svc, _, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	ctx := context.Background()
	svc.ArmReporterNoteForTest(ctx, inc.ID, time.Now())
	if len(notif.msgs) != 0 {
		t.Fatalf("msgs = %q, want none", notif.msgs)
	}
	if svc.HasProgressTimerForTest(inc.ID) {
		t.Fatal("a progress timer was armed with no new action")
	}
}

func TestE6_RestartRearmsUnsentAction(t *testing.T) {
	t.Parallel()
	_, database, inc, _ := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	base := time.Now().Add(-10 * time.Minute)
	if _, err := database.ClaimReporterNote(ctx, inc.ID, base, 0); err != nil {
		t.Fatal(err)
	}
	logAppliedAt(t, database, inc.ID, "refresh_decypharr_links", base.Add(time.Minute))

	notif2 := &captureNotifier{}
	svc2 := incident.NewService(
		context.Background(), database, nil, nil, nil, nil, notif2, slog.New(slog.DiscardHandler),
	)
	t.Cleanup(func() { stopService(t, svc2) })

	svc2.RearmReporterNotesForTest(ctx)
	if !svc2.HasProgressTimerForTest(inc.ID) {
		t.Fatal("the restart did not re-arm the unsent action's deferred note")
	}
	svc2.FireProgressTimerForTest(ctx, inc.ID, base.Add(15*time.Minute))

	want := []string{"⏳ Still working on **X**: fixing the media storage."}
	if strings.Join(notif2.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif2.msgs, want)
	}
}

func TestE6_NonProgressStateSendsNothing(t *testing.T) {
	t.Parallel()
	for _, status := range []db.IncidentStatus{db.StatusManualTestNeeded, db.StatusResolved} {
		svc, database, inc, notif := newProgressIncident(t, status)
		ctx := context.Background()
		logApplied(t, database, inc.ID, "restart_jellyfin")
		svc.ArmReporterNoteForTest(ctx, inc.ID, time.Now())
		if len(notif.msgs) != 0 {
			t.Fatalf("%s: msgs = %q, want none", status, notif.msgs)
		}
	}
}

func TestE6_ActionPathTriggersNote(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	if _, err := database.ClaimReporterNote(ctx, inc.ID, base, 0); err != nil {
		t.Fatal(err)
	}
	logAppliedAt(t, database, inc.ID, "refresh_decypharr_links", base.Add(20*time.Minute))

	finished := lifecycle.Event{Kind: lifecycle.AttemptFinished, Gen: inc.Generation}
	if _, err := svc.ApplyForTest(ctx, inc.ID, finished); err != nil {
		t.Fatal(err)
	}

	want := []string{"⏳ Still working on **X**: fixing the media storage."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif.msgs, want)
	}
}

func TestE6_DownloadProgressSendsNote(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusAwaitingDownload)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	if _, err := database.ClaimReporterNote(ctx, inc.ID, time.Now().Add(-time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	snap := lifecycletest.Force(t, database, inc.ID, func(n *lifecycle.Snapshot) {
		n.Download.GrabNotified = true
		n.Download.StartedAt = time.Now()
		n.Download.LastProgressAt = time.Now().Add(-2 * time.Hour)
		n.Download.LastProgressPct = 10
	})

	obs := &lifecycle.Observation{InQueue: true, QueueStatus: "downloading", ProgressPct: 40}
	if _, err := svc.ApplyForTest(ctx, inc.ID, lifecycle.Event{
		Kind: lifecycle.DownloadObserved, Gen: snap.Generation, Obs: obs,
	}); err != nil {
		t.Fatal(err)
	}

	want := []string{"⏳ Still working on **X**: downloading a new copy (40%)."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif.msgs, want)
	}
}

func TestE6_ShutdownDropsDeferredNote(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	base := time.Now()
	if _, err := database.ClaimReporterNote(ctx, inc.ID, base, 0); err != nil {
		t.Fatal(err)
	}
	logAppliedAt(t, database, inc.ID, "refresh_decypharr_links", base.Add(time.Minute))
	svc.ArmReporterNoteForTest(ctx, inc.ID, base.Add(time.Minute))
	if !svc.HasProgressTimerForTest(inc.ID) {
		t.Fatal("no deferred note was armed inside the gap")
	}

	stopService(t, svc)

	if svc.HasProgressTimerForTest(inc.ID) {
		t.Fatal("shutdown left the deferred progress note armed")
	}
	if len(notif.msgs) != 0 {
		t.Fatalf("msgs = %q, want none", notif.msgs)
	}
}

type midRunActionAgent struct {
	*escalationAgent

	db      *db.DB
	logged  chan struct{}
	release chan struct{}
}

func (a *midRunActionAgent) Run(
	ctx context.Context, inc *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	if err := gate.Claim(ctx); err != nil {
		return agent.RunExit{State: agent.RunNotClaimed}
	}
	err := a.db.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "refresh_decypharr_links", TriggeredBy: "agent", Status: db.ActionApplied,
	})
	if err != nil {
		return agent.RunExit{State: agent.RunNotClaimed}
	}
	gate.ActionLogged(ctx)
	close(a.logged)
	<-a.release
	return gatedExit(ctx, gate, nil, nil, ctx.Err())
}

func TestF2_MidRunActionSendsNote(t *testing.T) {
	t.Parallel()
	_, database, _ := newTestService(t)
	ag := &midRunActionAgent{
		escalationAgent: newEscalationAgent(), db: database,
		logged: make(chan struct{}), release: make(chan struct{}),
	}
	notif := newSyncNotifier()
	svc := incident.NewService(context.Background(), database, nil, ag, nil, nil, notif, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopService(t, svc) })
	released := false
	release := func() {
		if !released {
			released = true
			close(ag.release)
		}
	}
	t.Cleanup(release)

	if _, err := svc.Handle(context.Background(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "X",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ag.logged:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("the run never logged its action")
	}

	want := "⏳ Still working on **X**: fixing the media storage."
	select {
	case got := <-notif.userMsgs:
		if got != want {
			t.Errorf("note = %q, want %q", got, want)
		}
	case <-time.After(notifyWaitTimeout):
		t.Error("no progress note arrived while the run was still going")
	}
	release()
}

type cancelledLogAgent struct {
	*escalationAgent

	db *db.DB
}

func (a *cancelledLogAgent) Run(
	ctx context.Context, inc *db.Incident, _ []openai.ChatCompletionMessage, gate agent.Gate,
) agent.RunExit {
	if err := gate.Claim(ctx); err != nil {
		return agent.RunExit{State: agent.RunNotClaimed}
	}
	err := a.db.LogAction(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "refresh_decypharr_links", TriggeredBy: "agent", Status: db.ActionApplied,
	})
	if err != nil {
		return agent.RunExit{State: agent.RunNotClaimed}
	}
	runCtx, cancel := context.WithCancel(ctx)
	cancel()
	gate.ActionLogged(runCtx)
	return gatedExit(ctx, gate, nil, nil, nil)
}

func TestF36_ReporterNoteSurvivesRunCancel(t *testing.T) {
	t.Parallel()
	_, database, _ := newTestService(t)
	ag := &cancelledLogAgent{escalationAgent: newEscalationAgent(), db: database}
	notif := newSyncNotifier()
	svc := incident.NewService(context.Background(), database, nil, ag, nil, nil, notif, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopService(t, svc) })

	if _, err := svc.Handle(context.Background(), &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice",
		What: "cant_play", Title: "X",
	}); err != nil {
		t.Fatal(err)
	}

	want := "⏳ Still working on **X**: fixing the media storage."
	select {
	case got := <-notif.userMsgs:
		if got != want {
			t.Errorf("note = %q, want %q", got, want)
		}
	case <-time.After(notifyWaitTimeout):
		t.Error("the reporter note was lost when the run's context was cancelled mid-delivery")
	}
}
