package incident

import (
	"context"
	"os"
	"testing"

	"io"
	"log/slog"

	"github.com/minz1/mediafixer/internal/db"
)

type captureNotifier struct{ msgs []string }

func (c *captureNotifier) NotifyOwner(_ context.Context, msg string) error {
	c.msgs = append(c.msgs, msg)
	return nil
}

func (c *captureNotifier) NotifyUser(_ context.Context, _, msg string) error {
	c.msgs = append(c.msgs, msg)
	return nil
}

func newTestService(t *testing.T) (*Service, *db.DB, *captureNotifier) {
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

	notif := &captureNotifier{}
	// agent is nil — tests must not trigger the agent goroutine, so all
	// incidents are created with a nil agent and the goroutine exits early.
	svc := NewService(database, nil, nil, notif, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, database, notif
}

func TestHandle_CreatesIncident(t *testing.T) {
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	rep := &Report{
		Source: "seerr", ReportedBy: "alice",
		What: "cant_play", Title: "Breaking Bad",
	}
	inc, err := svc.Handle(ctx, rep)
	if err != nil {
		t.Fatal(err)
	}
	if inc.ID == "" {
		t.Fatal("expected ID")
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "seerr" {
		t.Errorf("source: %q", got.Source)
	}
	if got.Status != db.StatusOpen {
		t.Errorf("status: %q", got.Status)
	}
}

func TestHandle_DeduplicatesByTitle(t *testing.T) {
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	rep := &Report{Source: "seerr", ReportedBy: "alice", What: "cant_play", Title: "Sopranos"}

	inc1, err := svc.Handle(ctx, rep)
	if err != nil {
		t.Fatal(err)
	}

	rep.ReportedBy = "bob"
	inc2, err := svc.Handle(ctx, rep)
	if err != nil {
		t.Fatal(err)
	}

	if inc1.ID != inc2.ID {
		t.Errorf("expected dedup: got %q and %q", inc1.ID, inc2.ID)
	}

	reporters, err := database.ListReporters(ctx, inc1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) != 2 {
		t.Errorf("reporters: got %d want 2", len(reporters))
	}
}

func TestHandle_SystemicLock(t *testing.T) {
	svc, database, notif := newTestService(t)
	ctx := context.Background()

	// Create 5 open incidents to hit the threshold.
	titles := []string{"A", "B", "C", "D", "E"}
	for _, title := range titles {
		if _, err := svc.Handle(ctx, &Report{
			Source: "seerr", ReportedBy: "x",
			What: "cant_play", Title: title,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The 6th should be locked.
	inc, err := svc.Handle(ctx, &Report{
		Source: "seerr", ReportedBy: "x",
		What: "cant_play", Title: "F",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutonomousLocked {
		t.Error("expected autonomous_locked for 6th incident")
	}
	if len(notif.msgs) == 0 {
		t.Error("expected owner notification for systemic lock")
	}
}

func TestResolveAndReopen(t *testing.T) {
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	inc, err := svc.Handle(ctx, &Report{
		Source: "discord", ReportedBy: "alice",
		What: "cant_play", Title: "Deadwood",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Resolve(ctx, inc.ID); err != nil {
		t.Fatal(err)
	}

	got, err := database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusResolved {
		t.Errorf("status after resolve: %q", got.Status)
	}

	// Reopen with nil agent just marks it reopened (agent goroutine exits immediately).
	if err := svc.Reopen(ctx, inc.ID); err != nil {
		t.Fatal(err)
	}
	got, err = database.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusReopened {
		t.Errorf("status after reopen: %q", got.Status)
	}
}

func TestSetAutonomousPaused(t *testing.T) {
	svc, database, _ := newTestService(t)
	ctx := context.Background()

	paused, _ := database.IsAutonomousPaused(ctx)
	if paused {
		t.Error("should start unpaused")
	}

	if err := svc.SetAutonomousPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	paused, _ = database.IsAutonomousPaused(ctx)
	if !paused {
		t.Error("should be paused")
	}

	if err := svc.SetAutonomousPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	paused, _ = database.IsAutonomousPaused(ctx)
	if paused {
		t.Error("should be unpaused again")
	}
}
