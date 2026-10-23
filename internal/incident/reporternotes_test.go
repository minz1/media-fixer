package incident_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

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
