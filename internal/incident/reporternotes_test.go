package incident_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

const (
	escalationNote = "🧑‍🔧 Handed **Lost** to minz1. You'll hear back here."
	rerunNote      = "🔁 Taking another look at **Lost**."
)

func seerrReport(issue string) *incident.Report {
	return &incident.Report{
		Source: "seerr", ReportedBy: "alice", What: "cant_play", Title: "Breaking Bad", SeerrIssueID: issue,
	}
}

func countMsgs(msgs []string, want string) int {
	n := 0
	for _, m := range msgs {
		if m == want {
			n++
		}
	}
	return n
}

func TestIntakeNote_NewSeerrIncident(t *testing.T) {
	t.Parallel()
	svc, _, notif := newTestService(t)
	seerr := &fakeSeerr{}
	svc.SetSeerr(seerr)

	if _, err := svc.Handle(context.Background(), seerrReport("7")); err != nil {
		t.Fatal(err)
	}

	want := []string{"👀 Thanks, we're looking into **Breaking Bad**."}
	if got := seerr.comments["7"]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("comments on 7 = %q, want %q", got, want)
	}
	if len(notif.msgs) != 0 {
		t.Errorf("notifier got %q, want nothing", notif.msgs)
	}
}

func TestIntakeNote_CollapsedSecondIssue(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	seerr := &fakeSeerr{}
	svc.SetSeerr(seerr)
	ctx := context.Background()

	if _, err := svc.Handle(ctx, seerrReport("7")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Handle(ctx, seerrReport("8")); err != nil {
		t.Fatal(err)
	}

	if len(seerr.comments["7"]) != 1 || len(seerr.comments["8"]) != 1 {
		t.Fatalf("comments = %q, want one on each of 7 and 8", seerr.comments)
	}
	if !strings.HasPrefix(seerr.comments["8"][0], "👀") {
		t.Errorf("comment on 8 = %q, want the intake note", seerr.comments["8"][0])
	}
}

func TestIntakeNote_ReopenAttachedIssue(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	seerr := &fakeSeerr{}
	svc.SetSeerr(seerr)
	ctx := context.Background()

	if _, err := svc.Handle(ctx, seerrReport("7")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Handle(ctx, seerrReport("7")); err != nil {
		t.Fatal(err)
	}

	if len(seerr.comments["7"]) != 1 {
		t.Errorf("comments on 7 = %q, want only the first intake note", seerr.comments["7"])
	}
}

func TestIntakeNote_SeerrFailureDoesNotBlockIncident(t *testing.T) {
	t.Parallel()
	svc, database, notif := newTestService(t)
	svc.SetSeerr(&fakeSeerr{err: errors.New("seerr down")})
	ctx := context.Background()

	inc, err := svc.Handle(ctx, seerrReport("7"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err = database.GetIncident(ctx, inc.ID); err != nil {
		t.Fatalf("incident not created: %v", err)
	}
	reporters, err := database.ListReporters(ctx, inc.ID)
	if err != nil || len(reporters) != 1 {
		t.Fatalf("reporters = %v, %v; want one", reporters, err)
	}
	if len(notif.msgs) != 1 || !strings.Contains(notif.msgs[0], "Could not comment on Seerr issue 7") {
		t.Errorf("owner msgs = %q, want one Could not comment DM", notif.msgs)
	}
}

func TestRerunNote_RateLimited(t *testing.T) {
	t.Parallel()
	svc, _, notif := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "alice", ReporterDiscordID: "discord-alice", What: "cant_play", Title: "Lost",
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = svc.Rerun(ctx, inc.ID); err != nil {
			t.Fatal(err)
		}
	}

	if n := countMsgs(notif.msgs, rerunNote); n != 1 {
		t.Errorf("rerun notes = %d, want 1; msgs %q", n, notif.msgs)
	}
	for _, m := range notif.msgs {
		if strings.Contains(m, "re-run") {
			t.Errorf("owner got %q for a manual rerun, want no owner DM", m)
		}
	}
}

func newEscalationService(t *testing.T, status db.IncidentStatus) (*incident.Service, *db.Incident, *syncNotifier) {
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
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "Lost"}
	if err = database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err = database.AddReporter(ctx, inc.ID, "alice", "discord", "discord-alice"); err != nil {
		t.Fatal(err)
	}
	if err = database.UpdateIncidentStatus(ctx, inc.ID, status); err != nil {
		t.Fatal(err)
	}
	notif := newSyncNotifier()
	svc := incident.NewService(context.Background(), database, nil, nil, nil, nil, notif, slog.New(slog.DiscardHandler))
	return svc, inc, notif
}

func TestEscalationNote_Once(t *testing.T) {
	t.Parallel()
	svc, inc, notif := newEscalationService(t, db.StatusInvestigating)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { svc.EscalateToOwnerForTest(context.Background(), inc, "owner: look at Lost") })
	}
	wg.Wait()

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if n := countMsgs(notif.msgs, escalationNote); n != 1 {
		t.Errorf("reporter notes = %d, want 1; msgs %q", n, notif.msgs)
	}
	if n := countMsgs(notif.msgs, "owner: look at Lost"); n != 1 {
		t.Errorf("owner DMs = %d, want 1; msgs %q", n, notif.msgs)
	}
}

func TestEscalationNote_SkippedAfterAgentFixed(t *testing.T) {
	t.Parallel()
	svc, inc, notif := newEscalationService(t, db.StatusAgentFixed)

	svc.EscalateToOwnerForTest(context.Background(), inc, "owner: look at Lost")

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.msgs) != 1 || notif.msgs[0] != "owner: look at Lost" {
		t.Errorf("msgs = %q, want only the owner DM", notif.msgs)
	}
}

type detachedSeerr struct {
	fakeSeerr

	cancellable bool
}

func (d *detachedSeerr) Comment(ctx context.Context, issueID, msg string) error {
	d.cancellable = ctx.Done() != nil
	return d.fakeSeerr.Comment(ctx, issueID, msg)
}

func TestIntakeNote_OutlivesTheWebhookRequest(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	seerr := &detachedSeerr{}
	svc.SetSeerr(seerr)
	if _, err := svc.Handle(t.Context(), seerrReport("7")); err != nil {
		t.Fatal(err)
	}
	if len(seerr.comments["7"]) != 1 {
		t.Fatalf("comments on 7 = %q, want one", seerr.comments["7"])
	}
	if seerr.cancellable {
		t.Error("intake comment used the request context; a dropped webhook would cancel it")
	}
}

func TestSeerrFailureDM_OncePerHour(t *testing.T) {
	t.Parallel()
	svc, _, notif := newTestService(t)
	svc.SetSeerr(&fakeSeerr{err: errors.New("seerr down")})
	ctx := context.Background()

	for _, title := range []string{"Lost", "Fringe"} {
		r := seerrReport("7")
		r.Title = title
		if _, err := svc.Handle(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if len(notif.msgs) != 1 {
		t.Fatalf("owner msgs = %q, want one Seerr failure DM per hour", notif.msgs)
	}
	if !strings.Contains(notif.msgs[0], "in the next 1 hour are only logged") {
		t.Errorf("DM = %q, want the 1 hour hint", notif.msgs[0])
	}

	svc.BackdateSeerrAlertForTest(time.Hour)
	r := seerrReport("9")
	r.Title = "Dark"
	if _, err := svc.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(notif.msgs) != 2 {
		t.Errorf("owner msgs = %q, want a second DM after an hour", notif.msgs)
	}
}

func TestRerunNote_NotOnRestartRecovery(t *testing.T) {
	t.Parallel()
	svc, _, notif := newEscalationService(t, db.StatusInvestigating)

	svc.RecoverZombies(context.Background())

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if n := countMsgs(notif.msgs, rerunNote); n != 0 {
		t.Errorf("rerun notes = %d, want none after a restart; msgs %q", n, notif.msgs)
	}
	if len(notif.msgs) != 0 {
		t.Errorf("msgs = %q, want nothing after a restart", notif.msgs)
	}
}

func TestSeerrAlert_FailedDMRetriesNext(t *testing.T) {
	t.Parallel()
	svc, _, notif := newTestService(t)
	notif.ownerFails = 1
	svc.SetSeerr(&fakeSeerr{err: errors.New("seerr down")})
	for _, title := range []string{"Lost", "Fringe"} {
		r := seerrReport("7")
		r.Title = title
		if _, err := svc.Handle(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, m := range notif.msgs {
		if strings.Contains(m, "Could not") {
			n++
		}
	}
	if n != 1 || len(notif.msgs) != 1 {
		t.Errorf("owner Seerr alerts delivered = %d, want 1 (the first DM failed, the next retries); msgs %q",
			n, notif.msgs)
	}
}

func TestSeerrFailureDM_SavedGap(t *testing.T) {
	t.Parallel()
	svc, database, notif := newTestService(t)
	saveTimers(t, database, func(tm *incident.Timers) { tm.SeerrAlertGapMinutes = 15 })
	svc.SetSeerr(&fakeSeerr{err: errors.New("seerr down")})
	ctx := context.Background()

	if _, err := svc.Handle(ctx, seerrReport("7")); err != nil {
		t.Fatal(err)
	}
	if len(notif.msgs) != 1 ||
		!strings.Contains(notif.msgs[0], "Further Seerr failures in the next 15 minutes are only logged.") {
		t.Fatalf("owner msgs = %q, want one DM with the 15 minute hint", notif.msgs)
	}

	svc.BackdateSeerrAlertForTest(16 * time.Minute)
	if _, err := svc.Handle(ctx, seerrReport("9")); err != nil {
		t.Fatal(err)
	}
	if len(notif.msgs) != 2 {
		t.Errorf("owner msgs = %q, want a second DM after 16 minutes", notif.msgs)
	}
}

func TestRerunNote_SavedGap(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	saveTimers(t, database, func(tm *incident.Timers) { tm.RerunNoteGapMinutes = 5 })
	ctx := context.Background()
	if _, err := database.ClaimReporterNote(ctx, inc.ID, time.Now().Add(-10*time.Minute), 0); err != nil {
		t.Fatal(err)
	}

	if err := svc.Rerun(ctx, inc.ID); err != nil {
		t.Fatal(err)
	}

	if n := countMsgs(notif.msgs, "🔁 Taking another look at **X**."); n != 1 {
		t.Errorf("rerun notes = %d, want 1 with a saved 5 minute gap; msgs %q", n, notif.msgs)
	}
}

func TestSeerrAlert_ReleaseKeepsNewerClaim(t *testing.T) {
	t.Parallel()
	svc, _, _ := newTestService(t)
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	prev, ok := svc.ClaimSeerrAlertForTest(at, time.Hour)
	if !ok {
		t.Fatal("first claim refused")
	}
	if _, ok = svc.ClaimSeerrAlertForTest(at.Add(2*time.Hour), time.Hour); !ok {
		t.Fatal("claim two hours later refused")
	}
	svc.ReleaseSeerrAlertForTest(at, prev)
	if got := svc.SeerrAlertAtForTest(); !got.Equal(at.Add(2 * time.Hour)) {
		t.Errorf("stamp after releasing the older claim = %v, want %v", got, at.Add(2*time.Hour))
	}
}
