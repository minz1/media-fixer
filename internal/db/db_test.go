package db

import (
	"context"
	"os"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateAndGetIncident(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	inc := &Incident{
		Status:     StatusOpen,
		Source:     "discord",
		ReportedBy: "alice",
		What:       "cant_play",
		Title:      "Breaking Bad",
		Details:    "won't load",
	}
	if err := db.CreateIncident(ctx, inc); err != nil {
		t.Fatalf("create: %v", err)
	}
	if inc.ID == "" {
		t.Fatal("expected ID to be set")
	}

	got, err := db.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != inc.Title {
		t.Errorf("title: got %q want %q", got.Title, inc.Title)
	}
	if got.Status != StatusOpen {
		t.Errorf("status: got %q want %q", got.Status, StatusOpen)
	}
}

func TestFindOpenByTitle_Dedup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	inc := &Incident{
		Status:     StatusOpen,
		Source:     "seerr",
		ReportedBy: "bob",
		What:       "cant_play",
		Title:      "The Wire",
	}
	if err := db.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	found, err := db.FindOpenByTitle(ctx, "The Wire")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("expected to find open incident")
	}
	if found.ID != inc.ID {
		t.Errorf("got %q want %q", found.ID, inc.ID)
	}

	// Resolved incidents should not match.
	if err := db.UpdateIncidentStatus(ctx, inc.ID, StatusResolved); err != nil {
		t.Fatal(err)
	}
	found, err = db.FindOpenByTitle(ctx, "The Wire")
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Error("expected nil for resolved incident")
	}
}

func TestCountOpenIncidents(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := range 3 {
		if err := db.CreateIncident(ctx, &Incident{
			Status:     StatusOpen,
			Source:     "discord",
			ReportedBy: "user",
			What:       "cant_play",
			Title:      string(rune('A' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.CountOpenIncidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("count: got %d want 3", n)
	}
}

func TestIncrementActionCount(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	inc := &Incident{
		Status: StatusOpen, Source: "discord",
		ReportedBy: "x", What: "cant_play", Title: "T",
	}
	if err := db.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	for want := range 3 {
		n, err := db.IncrementActionCount(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n != want+1 {
			t.Errorf("increment %d: got %d", want+1, n)
		}
	}
}

func TestLogAndListActions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	inc := &Incident{
		Status: StatusOpen, Source: "discord",
		ReportedBy: "x", What: "cant_play", Title: "T",
	}
	if err := db.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	a := &ActionLog{
		IncidentID:  inc.ID,
		Action:      "refresh_links",
		Params:      map[string]any{"torrent": "foo"},
		TriggeredBy: "agent",
		Status:      ActionApplied,
	}
	if err := db.LogAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" {
		t.Fatal("expected ID")
	}

	now := time.Now()
	if err := db.UpdateAction(ctx, a.ID, ActionApplied, "run_id=abc", ""); err != nil {
		t.Fatal(err)
	}

	actions, err := db.ListActions(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions want 1", len(actions))
	}
	got := actions[0]
	if got.Action != "refresh_links" {
		t.Errorf("action: %q", got.Action)
	}
	if got.AppliedAt == nil || got.AppliedAt.Before(now.Add(-time.Second)) {
		t.Error("applied_at not set correctly")
	}
}

func TestReporters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	inc := &Incident{
		Status: StatusOpen, Source: "discord",
		ReportedBy: "a", What: "cant_play", Title: "T",
	}
	if err := db.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	for _, r := range []string{"alice", "bob", "alice"} { // alice twice → dedup
		if err := db.AddReporter(ctx, inc.ID, r, "discord"); err != nil {
			t.Fatal(err)
		}
	}

	reporters, err := db.ListReporters(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) != 2 {
		t.Errorf("reporters: got %d want 2", len(reporters))
	}
}

func TestSettings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	paused, err := db.IsAutonomousPaused(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if paused {
		t.Error("should not be paused by default")
	}

	if err := db.SetSetting(ctx, "autonomous_paused", "true"); err != nil {
		t.Fatal(err)
	}
	paused, err = db.IsAutonomousPaused(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Error("should be paused after setting")
	}
}

func TestSetIncidentFinding(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	inc := &Incident{
		Status: StatusOpen, Source: "discord",
		ReportedBy: "x", What: "cant_play", Title: "T",
	}
	if err := db.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	finding := map[string]any{"root_cause": "stale links", "confidence": "high"}
	actions := map[string]any{"primary": "refresh_links"}
	if err := db.SetIncidentFinding(ctx, inc.ID, finding, actions); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.Finding.(map[string]any)
	if !ok {
		t.Fatalf("finding type: %T", got.Finding)
	}
	if m["root_cause"] != "stale links" {
		t.Errorf("root_cause: %v", m["root_cause"])
	}
}
