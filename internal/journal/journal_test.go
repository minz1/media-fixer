package journal_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

func openTestJournal(t *testing.T) (*journal.Journal, *db.DB) {
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
	return journal.New(d), d
}

// mustCreateIncident creates a bare incident, satisfying incident_events' and
// actions_log's foreign key to incidents(id) — required now that foreign key
// enforcement is genuinely on (see internal/db's Phase 0 fix).
func mustCreateIncident(t *testing.T, d *db.DB, title string) *db.Incident {
	t.Helper()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: title}
	if err := d.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return inc
}

func TestLogAction_WritesEventAndProjection(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")

	action := &db.ActionLog{
		IncidentID:  inc.ID,
		Action:      "clear_jellyfin_cache",
		Params:      map[string]any{"item_id": "abc"},
		TriggeredBy: "agent",
		Status:      db.ActionApplied,
	}
	if err := j.LogAction(ctx, action, false); err != nil {
		t.Fatal(err)
	}
	if action.ID == "" {
		t.Fatal("expected LogAction to assign an ID")
	}

	// The event exists...
	events, err := j.Since(ctx, inc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != string(journal.KindActionApplied) {
		t.Fatalf("got %d events, want 1 action_applied event: %+v", len(events), events)
	}

	// ...and its actions_log projection agrees with it, sharing the same ID.
	actions, err := d.ListActions(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions_log rows, want 1", len(actions))
	}
	if actions[0].ID != action.ID {
		t.Errorf("actions_log.id %q does not match the event's action ID %q", actions[0].ID, action.ID)
	}
	if actions[0].Action != "clear_jellyfin_cache" {
		t.Errorf("actions_log.action = %q", actions[0].Action)
	}
}

func TestLogAction_RejectsUnknownIncident_LeavesNoTrace(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()

	err := j.LogAction(ctx, &db.ActionLog{
		IncidentID: "does-not-exist", Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied,
	}, false)
	if err == nil {
		t.Fatal("expected an error logging an action against a nonexistent incident (foreign key enforcement)")
	}

	// Nothing left behind — the event and its projection are one transaction;
	// a rejected write must not leave an orphaned event row.
	events, evErr := j.Since(ctx, "does-not-exist", 0)
	if evErr != nil {
		t.Fatal(evErr)
	}
	if len(events) != 0 {
		t.Fatalf("got %d leftover event(s) after a rejected LogAction, want 0", len(events))
	}
	actions, actErr := d.ListActions(ctx, "does-not-exist")
	if actErr != nil {
		t.Fatal(actErr)
	}
	if len(actions) != 0 {
		t.Fatalf("got %d leftover actions_log row(s) after a rejected LogAction, want 0", len(actions))
	}
}

func TestLastDisruption_NoneRecorded(t *testing.T) {
	t.Parallel()
	j, _ := openTestJournal(t)
	if _, err := j.LastDisruption(context.Background()); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before any disruption recorded, got %v", err)
	}
}

func TestLastDisruption_OnlyDisruptiveTaggedCounts(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc1 := mustCreateIncident(t, d, "One")
	inc2 := mustCreateIncident(t, d, "Two")

	// A non-disruptive action must not register as a disruption.
	if err := j.LogAction(ctx, &db.ActionLog{
		IncidentID: inc1.ID, Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied,
	}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := j.LastDisruption(ctx); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after only a non-disruptive action, got %v", err)
	}

	if logErr := j.LogAction(ctx, &db.ActionLog{
		IncidentID: inc1.ID, Action: "restart_jellyfin", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true); logErr != nil {
		t.Fatal(logErr)
	}
	got, err := j.LastDisruption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "restart_jellyfin" || got.IncidentID != inc1.ID {
		t.Fatalf("got %+v", got)
	}

	// A second incident's disruptive action supersedes the first as "most recent".
	if logErr := j.LogAction(ctx, &db.ActionLog{
		IncidentID: inc2.ID, Action: "jellyfin_library_scan", TriggeredBy: "agent", Status: db.ActionApplied,
	}, true); logErr != nil {
		t.Fatal(logErr)
	}
	got, err = j.LastDisruption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "jellyfin_library_scan" || got.IncidentID != inc2.ID {
		t.Fatalf("second disruption did not supersede the first: got %+v", got)
	}
}

func TestConversation_NotFoundBeforeAnyRun(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	inc := mustCreateIncident(t, d, "T")
	if _, err := j.Conversation(context.Background(), inc.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestConversation_ReconstructsSeedPlusRounds(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")

	seed := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system prompt"},
		{Role: openai.ChatMessageRoleUser, Content: "incident details"},
	}
	if err := j.RunStarted(ctx, inc.ID, seed); err != nil {
		t.Fatal(err)
	}
	round0Assistant := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "checking..."}
	round0Tools := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleTool, Content: `{"ok":true}`, ToolCallID: "call1"},
	}
	if err := j.LLMRound(ctx, inc.ID, 0, round0Assistant, round0Tools); err != nil {
		t.Fatal(err)
	}
	round1Assistant := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "done"}
	if err := j.LLMRound(ctx, inc.ID, 1, round1Assistant, nil); err != nil {
		t.Fatal(err)
	}

	got, err := j.Conversation(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]openai.ChatCompletionMessage{}, seed...), round0Assistant), round0Tools...)
	want = append(want, round1Assistant)
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Errorf("message %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestConversation_OnlyLatestRunSurvives is the regression test for the old
// conversation_history table's actual semantics (a single row upserted by
// incident_id, so a rerun's conversation always overwrote — not appended to —
// the previous run's): a second RunStarted (a rerun) must reset Conversation
// to start folding from it, not concatenate onto the first run's rounds.
func TestConversation_OnlyLatestRunSurvives(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")

	firstSeed := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "first run"}}
	if err := j.RunStarted(ctx, inc.ID, firstSeed); err != nil {
		t.Fatal(err)
	}
	if err := j.LLMRound(ctx, inc.ID, 0,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "first run's round"}, nil,
	); err != nil {
		t.Fatal(err)
	}

	secondSeed := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "rerun, summarized"}}
	if err := j.RunStarted(ctx, inc.ID, secondSeed); err != nil {
		t.Fatal(err)
	}
	secondRound := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "second run's round"}
	if err := j.LLMRound(ctx, inc.ID, 0, secondRound, nil); err != nil {
		t.Fatal(err)
	}

	got, err := j.Conversation(ctx, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []openai.ChatCompletionMessage{secondSeed[0], secondRound}
	if len(got) != len(want) {
		t.Fatalf("got %d messages (leaked the first run?): %+v", len(got), got)
	}
	for i := range want {
		if got[i].Content != want[i].Content {
			t.Errorf("message %d: got %q, want %q", i, got[i].Content, want[i].Content)
		}
	}
}

func TestSince_ReturnsEventsInSeqOrderAfterCursor(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")

	if err := j.IncidentCreated(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if err := j.ReporterAdded(ctx, inc.ID, "alice", "discord", "123"); err != nil {
		t.Fatal(err)
	}
	if err := j.RunFinished(ctx, inc.ID, 3, nil); err != nil {
		t.Fatal(err)
	}

	all, err := j.Since(ctx, inc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	if all[0].Seq >= all[1].Seq || all[1].Seq >= all[2].Seq {
		t.Fatalf("events not in ascending seq order: %+v", all)
	}

	rest, err := j.Since(ctx, inc.ID, all[0].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 || rest[0].Seq != all[1].Seq {
		t.Fatalf("Since(afterSeq) did not resume correctly: got %+v", rest)
	}
}

func TestSubscribe_ReceivesAppendedEventsAndUnsubscribes(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc := mustCreateIncident(t, d, "T")

	ch, unsubscribe := j.Subscribe(inc.ID)

	if err := j.RunFinished(ctx, inc.ID, 1, nil); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-ch:
		if e.Kind != string(journal.KindRunFinished) {
			t.Errorf("got kind %q, want %q", e.Kind, journal.KindRunFinished)
		}
	case <-time.After(subscribeWaitTimeout):
		t.Fatal("timed out waiting for the subscribed event")
	}

	unsubscribe()

	// Appending after unsubscribe must not panic (send-on-closed-channel) or
	// block Append itself.
	if err := j.RunFinished(ctx, inc.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
}

const subscribeWaitTimeout = 2 * time.Second

func TestSubscribeAll_ReceivesEventsFromAnyIncident(t *testing.T) {
	t.Parallel()
	j, d := openTestJournal(t)
	ctx := context.Background()
	inc1 := mustCreateIncident(t, d, "One")
	inc2 := mustCreateIncident(t, d, "Two")

	ch, unsubscribe := j.SubscribeAll()
	defer unsubscribe()

	if err := j.RunFinished(ctx, inc1.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := j.RunFinished(ctx, inc2.ID, 2, nil); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for range 2 {
		select {
		case e := <-ch:
			seen[e.IncidentID] = true
		case <-time.After(subscribeWaitTimeout):
			t.Fatal("timed out waiting for an event")
		}
	}
	if !seen[inc1.ID] || !seen[inc2.ID] {
		t.Fatalf("expected events from both incidents, got %+v", seen)
	}
}
