package incident_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestF9_EscalationDMSurvivesDiscordOutage(t *testing.T) {
	t.Parallel()
	svc, database, notif := newTestService(t)
	ctx := context.Background()
	inc := &db.Incident{
		Status:     db.StatusOpen,
		Source:     "discord",
		ReportedBy: "fixture",
		What:       "cant_play",
		Title:      "Lost",
	}
	lifecycletest.Incident(t, database, inc)
	lifecycletest.MoveTo(t, database, inc.ID, db.StatusInvestigating)
	notif.ownerFails = 3
	svc.EscalateToOwnerForTest(ctx, inc, "owner: look at Lost")
	start := time.Now()
	for _, after := range []time.Duration{time.Minute, 6 * time.Minute} {
		svc.FlushOutboxForTest(ctx, start.Add(after))
		if n := countMsgs(notif.msgs, "owner: look at Lost"); n != 0 {
			t.Fatalf("delivered %d while Discord is down", n)
		}
	}
	svc.FlushOutboxForTest(ctx, start.Add(37*time.Minute))
	if n := countMsgs(notif.msgs, "owner: look at Lost"); n != 1 {
		t.Fatalf("owner DMs = %d after the 4th attempt, want 1; msgs %q", n, notif.msgs)
	}
}

func TestF16_SeerrCommentKeepsDiscordWindow(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	svc.SetSeerr(&fakeSeerr{})
	ctx := context.Background()
	logApplied(t, database, inc.ID, "refresh_decypharr_links")
	r := seerrReport("7")
	r.Title = "X"
	if _, err := svc.Handle(ctx, r); err != nil {
		t.Fatal(err)
	}
	svc.ArmReporterNoteForTest(ctx, inc.ID, time.Now().Add(61*time.Minute))
	want := "⏳ Still working on **X**: fixing the media storage."
	if n := countMsgs(notif.msgs, want); n != 1 {
		t.Fatalf("Discord progress notes = %d, want 1 (the Seerr comment must not hide the action); msgs %q",
			n, notif.msgs)
	}
}

func TestF17_UnreachableReporterOneOwnerDMPerDay(t *testing.T) {
	t.Parallel()
	svc, database, inc, notif := newProgressIncident(t, db.StatusInvestigating)
	notif.userErr = errors.New("no mutual guilds")
	ctx := context.Background()
	start := time.Now()
	for i := range 5 {
		at := start.Add(time.Duration(i) * 61 * time.Minute)
		logAppliedAt(t, database, inc.ID, "refresh_decypharr_links", at.Add(-time.Second))
		svc.ArmReporterNoteForTest(ctx, inc.ID, at)
	}
	n := 0
	for _, m := range notif.msgs {
		if strings.Contains(m, "Could not DM reporter") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("owner fallback DMs = %d, want 1 per reporter per day; msgs %q", n, notif.msgs)
	}
}
