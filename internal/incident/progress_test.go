package incident_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
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

	po := &db.PendingOutcome{LastStage: "downloading", LastProgressPct: 43.4}
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
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "X"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := database.AddReporter(ctx, inc.ID, "alice", "discord", "discord-alice"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateIncidentStatus(ctx, inc.ID, status); err != nil {
		t.Fatal(err)
	}
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
