package db_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	d, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestCreateAndGetIncident(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusOpen,
		Source:     "discord",
		ReportedBy: "alice",
		What:       "cant_play",
		Title:      "Breaking Bad",
		Details:    "won't load",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatalf("create: %v", err)
	}
	if inc.ID == "" {
		t.Fatal("expected ID to be set")
	}

	got, err := d.GetIncident(ctx, inc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != inc.Title {
		t.Errorf("title: got %q want %q", got.Title, inc.Title)
	}
	if got.Status != db.StatusOpen {
		t.Errorf("status: got %q want %q", got.Status, db.StatusOpen)
	}
}

func TestFindOpenByTitle_Dedup(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusOpen,
		Source:     "seerr",
		ReportedBy: "bob",
		What:       "cant_play",
		Title:      "The Wire",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	found, err := d.FindOpenByTitle(ctx, "The Wire")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != inc.ID {
		t.Errorf("got %q want %q", found.ID, inc.ID)
	}

	// Resolved incidents should not match.
	if updateErr := d.UpdateIncidentStatus(ctx, inc.ID, db.StatusResolved); updateErr != nil {
		t.Fatal(updateErr)
	}
	_, err = d.FindOpenByTitle(ctx, "The Wire")
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("expected ErrNotFound for resolved incident, got %v", err)
	}
}

const testIncidentCount = 3

func TestCountOpenIncidents(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	for i := range testIncidentCount {
		if err := d.CreateIncident(ctx, &db.Incident{
			Status:     db.StatusOpen,
			Source:     "discord",
			ReportedBy: "user",
			What:       "cant_play",
			Title:      string(rune('A' + i)),
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := d.CountOpenIncidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != testIncidentCount {
		t.Errorf("count: got %d want %d", n, testIncidentCount)
	}
}

func TestIncrementActionCount(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "x", What: "cant_play", Title: "T",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	for want := range testIncidentCount {
		n, err := d.IncrementActionCount(ctx, inc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n != want+1 {
			t.Errorf("increment %d: got %d", want+1, n)
		}
	}
}

func TestLogAndListActions(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "x", What: "cant_play", Title: "T",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	a := &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "refresh_links",
		Params:      map[string]any{"torrent": "foo"},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}
	if err := d.LogAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" {
		t.Fatal("expected ID")
	}

	now := time.Now()
	if err := d.UpdateAction(ctx, a.ID, db.ActionApplied, "run_id=abc", ""); err != nil {
		t.Fatal(err)
	}

	actions, err := d.ListActions(ctx, inc.ID)
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

const testReporterCount = 2

func TestReporters(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "a", What: "cant_play", Title: "T",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	for _, r := range []string{"alice", "bob", "alice"} { // alice twice → dedup
		if err := d.AddReporter(ctx, inc.ID, r, "discord", ""); err != nil {
			t.Fatal(err)
		}
	}

	reporters, err := d.ListReporters(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) != testReporterCount {
		t.Errorf("reporters: got %d want %d", len(reporters), testReporterCount)
	}
}

func TestSettings(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	paused, err := d.IsAutonomousPaused(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if paused {
		t.Error("should not be paused by default")
	}

	if setErr := d.SetSetting(ctx, "autonomous_paused", "true"); setErr != nil {
		t.Fatal(setErr)
	}
	paused, err = d.IsAutonomousPaused(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Error("should be paused after setting")
	}
}

func TestSetIncidentFinding(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "x", What: "cant_play", Title: "T",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	finding := map[string]any{"root_cause": "stale links", "confidence": "high"}
	actions := map[string]any{"primary": "refresh_links"}
	if err := d.SetIncidentFinding(ctx, inc.ID, finding, actions); err != nil {
		t.Fatal(err)
	}

	got, err := d.GetIncident(ctx, inc.ID)
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
