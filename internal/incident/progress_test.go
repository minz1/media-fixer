package incident_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

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

func TestAdvanceReporterProgress_NothingNew(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	ctx := context.Background()

	svc.AdvanceReporterProgress(ctx, time.Now())

	if len(notif.msgs) != 0 {
		t.Errorf("msgs = %q, want none", notif.msgs)
	}
	last, err := database.LastReporterNoteAt(ctx, inc.ID)
	if err != nil || !last.IsZero() {
		t.Errorf("last_reporter_note_at = %v, %v; want unset", last, err)
	}
}

func TestAdvanceReporterProgress_HourlyCap(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	ctx := context.Background()

	logApplied(t, database, inc.ID, "refresh_decypharr_links")
	start := time.Now()
	svc.AdvanceReporterProgress(ctx, start)
	logApplied(t, database, inc.ID, "sonarr_rescan")
	svc.AdvanceReporterProgress(ctx, start.Add(30*time.Minute))

	want := []string{"⏳ Still working on **X**: fixing the media storage."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif.msgs, want)
	}

	svc.AdvanceReporterProgress(ctx, start.Add(61*time.Minute))

	want = append(want, "⏳ Still working on **X**: updating the library.")
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Errorf("msgs = %q, want %q", notif.msgs, want)
	}
}

func TestAdvanceReporterProgress_OnlyActiveStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []db.IncidentStatus{db.StatusManualTestNeeded, db.StatusResolved} {
		svc, database, inc, notif := newProgressIncident(t, status)
		logApplied(t, database, inc.ID, "restart_jellyfin")

		svc.AdvanceReporterProgress(context.Background(), time.Now())

		if len(notif.msgs) != 0 {
			t.Errorf("%s: msgs = %q, want none", status, notif.msgs)
		}
	}
}

func TestAdvanceReporterProgress_SavedGap(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.ProgressNoteGapMinutes = 15 })
	ctx := context.Background()
	now := time.Now()
	if _, err := database.ClaimReporterNote(ctx, inc.ID, now.Add(-20*time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	logApplied(t, database, inc.ID, "refresh_decypharr_links")

	svc.AdvanceReporterProgress(ctx, now)

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
	svc2 := incident.NewService(context.Background(), database, nil, nil, nil, nil, notif2, slog.New(slog.DiscardHandler))
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

	if _, err := svc.ApplyForTest(ctx, inc.ID, lifecycle.Event{Kind: lifecycle.AttemptFinished, Gen: inc.Generation}); err != nil {
		t.Fatal(err)
	}

	want := []string{"⏳ Still working on **X**: fixing the media storage."}
	if strings.Join(notif.msgs, "|") != strings.Join(want, "|") {
		t.Fatalf("msgs = %q, want %q", notif.msgs, want)
	}
}
