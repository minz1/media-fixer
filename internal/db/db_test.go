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

func TestTransitionStatus_Idempotent(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "alice", What: "cant_play", Title: "T",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	allowed := []db.IncidentStatus{
		db.StatusOpen, db.StatusInvestigating, db.StatusVerifying, db.StatusReopened,
	}

	// First finisher wins the transition.
	changed, err := d.TransitionStatus(ctx, inc.ID, db.StatusAgentFixed, allowed...)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first transition open→agent_fixed should return true")
	}

	// Second finisher: already agent_fixed (not in allowedFrom) → no change, no notify.
	changed, err = d.TransitionStatus(ctx, inc.ID, db.StatusAgentFixed, allowed...)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("second transition should return false (already agent_fixed)")
	}

	// After a legitimate reopen, the transition is allowed again.
	if err = d.UpdateIncidentStatus(ctx, inc.ID, db.StatusReopened); err != nil {
		t.Fatal(err)
	}
	changed, err = d.TransitionStatus(ctx, inc.ID, db.StatusAgentFixed, allowed...)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("transition reopened→agent_fixed should return true")
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

// TestReporters_DedupeSameDiscordUserDifferentDisplayName reproduces the
// duplicate-DM bug: the same Discord user reports under two different display
// names (a nickname change between calls, or a retried /report interaction).
// The partial unique index on (incident_id, discord_user_id) must reject the
// second row at write time, so a single person is stored — and therefore
// notified — exactly once, regardless of which reader is used.
func TestReporters_DedupeSameDiscordUserDifferentDisplayName(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord",
		ReportedBy: "alice", What: "cant_play", Title: "T",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	if err := d.AddReporter(ctx, inc.ID, "alice", "discord", "discord-user-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddReporter(ctx, inc.ID, "alice-new-nick", "discord", "discord-user-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddReporter(ctx, inc.ID, "bob", "discord", "discord-user-2"); err != nil {
		t.Fatal(err)
	}

	// Structural guarantee: the second nick for discord-user-1 never became a row.
	reporters, err := d.ListReporters(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) != 2 {
		t.Errorf("reporter rows: got %d want 2 (alice + bob) — %v", len(reporters), reporters)
	}

	ids, err := d.ListDiscordReporterIDs(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids: got %d want 2 (one per unique discord user) — %v", len(ids), ids)
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

// TestFindStaleInvestigating is the regression test for the heartbeat/stale
// run detection: an incident that has been sitting in "investigating" since
// before staleBefore, with no heartbeat written, must be found; one whose
// heartbeat was touched after staleBefore must not, even though its
// updated_at (when it first entered "investigating") is older.
func TestFindStaleInvestigating(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	stale := &db.Incident{
		Status:     db.StatusInvestigating,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Stale",
	}
	if err := d.CreateIncident(ctx, stale); err != nil {
		t.Fatal(err)
	}
	fresh := &db.Incident{
		Status:     db.StatusInvestigating,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Fresh",
	}
	if err := d.CreateIncident(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	notInvestigating := &db.Incident{
		Status:     db.StatusOpen,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Open",
	}
	if err := d.CreateIncident(ctx, notInvestigating); err != nil {
		t.Fatal(err)
	}

	// fresh gets a heartbeat after the cutoff; stale gets none at all, so it
	// falls back to (also pre-cutoff) updated_at.
	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)
	if err := d.TouchHeartbeat(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}

	got, err := d.FindStaleInvestigating(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != stale.ID {
		ids := make([]string, len(got))
		for i, inc := range got {
			ids[i] = inc.Title
		}
		t.Fatalf("expected only %q, got %v", stale.Title, ids)
	}
}

// TestPendingOutcome_SetGetClear covers the basic CRUD lifecycle: not found
// before anything is set, round-trips through JSON correctly, and is really
// gone (not just cleared to a zero value) after ClearPendingOutcome.
func TestPendingOutcome_SetGetClear(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	inc := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Rick and Morty",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}

	if _, err := d.GetPendingOutcome(ctx, inc.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before anything is set, got %v", err)
	}

	po := &db.PendingOutcome{
		MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9,
		StartedAt: time.Now(), LastStage: "searching",
	}
	if err := d.SetPendingOutcome(ctx, inc.ID, po, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetPendingOutcome(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Rick and Morty" || got.Season != 9 || got.Episode != 9 || got.LastStage != "searching" {
		t.Fatalf("got %+v", got)
	}

	if clearErr := d.ClearPendingOutcome(ctx, inc.ID); clearErr != nil {
		t.Fatal(clearErr)
	}
	if _, getErr := d.GetPendingOutcome(ctx, inc.ID); !errors.Is(getErr, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after clearing, got %v", getErr)
	}
}

// TestFindDuePendingOutcomes_ScopedToVerifying is the regression test for the
// "escalated incident keeps getting swept" bug: a pending outcome whose
// incident has moved to manual_test_needed (escalated after a no-release
// timeout) must NOT be returned, even though its next-check time has passed
// and pending_outcome is still set — only Service.KeepSearching should
// resume sweeping it, by transitioning status back to verifying first.
func TestFindDuePendingOutcomes_ScopedToVerifying(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	due := &db.Incident{Status: db.StatusVerifying, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Due"}
	if err := d.CreateIncident(ctx, due); err != nil {
		t.Fatal(err)
	}
	notYetDue := &db.Incident{
		Status:     db.StatusVerifying,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "NotYetDue",
	}
	if err := d.CreateIncident(ctx, notYetDue); err != nil {
		t.Fatal(err)
	}
	escalated := &db.Incident{
		Status:     db.StatusManualTestNeeded,
		Source:     "discord",
		ReportedBy: "x",
		What:       "cant_play",
		Title:      "Escalated",
	}
	if err := d.CreateIncident(ctx, escalated); err != nil {
		t.Fatal(err)
	}

	po := &db.PendingOutcome{MediaType: "tv", Title: "x", Season: -1, Episode: -1, StartedAt: time.Now()}
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	if err := d.SetPendingOutcome(ctx, due.ID, po, past); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPendingOutcome(ctx, notYetDue.ID, po, future); err != nil {
		t.Fatal(err)
	}
	if err := d.SetPendingOutcome(ctx, escalated.ID, po, past); err != nil {
		t.Fatal(err)
	}

	got, err := d.FindDuePendingOutcomes(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != due.ID {
		ids := make([]string, len(got))
		for i, inc := range got {
			ids[i] = inc.Title
		}
		t.Fatalf("expected only %q, got %v", due.Title, ids)
	}
}

// TestLastDisruption_RecordAndFetch covers the single-row upsert behavior
// RecordDisruption/LastDisruption relies on: no rows before anything is
// recorded, and a second RecordDisruption call overwrites (not duplicates)
// the row so LastDisruption always reflects only the most recent action.
func TestLastDisruption_RecordAndFetch(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()

	if _, err := d.LastDisruption(ctx); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before any disruption recorded, got %v", err)
	}

	if err := d.RecordDisruption(ctx, "restart_jellyfin", "inc-1"); err != nil {
		t.Fatal(err)
	}
	got, err := d.LastDisruption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "restart_jellyfin" || got.IncidentID != "inc-1" {
		t.Fatalf("got %+v", got)
	}

	if recordErr := d.RecordDisruption(ctx, "jellyfin_library_scan", "inc-2"); recordErr != nil {
		t.Fatal(recordErr)
	}
	got, err = d.LastDisruption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "jellyfin_library_scan" || got.IncidentID != "inc-2" {
		t.Fatalf("second record did not overwrite the first: got %+v", got)
	}
}
