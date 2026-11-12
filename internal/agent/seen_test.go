package agent_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

const searchEpisode7 = `{"media_type":"tv","title":"Rick and Morty","season":9,"episode":7}`

func TestA1_5_RepeatOfFailedCallNotCountedSaysFailed(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.repairRunCode = http.StatusInternalServerError
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 1 {
		t.Fatalf("Act ran %d times, want once: %v", len(got), got)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) < 2 {
		t.Fatalf("replies = %v, want two", replies)
	}
	failed := strings.Contains(replies[1], "already tried in this run and it failed")
	if !failed || !strings.Contains(replies[1], "status 500") {
		t.Errorf("repeat reply = %q, want it to say the earlier call failed with its error", replies[1])
	}
	if strings.Contains(replies[1], "applied") {
		t.Errorf("repeat reply = %q, must not claim it was applied", replies[1])
	}
}

func TestA6_4_InRunRepeatCheckedBeforeCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label, tool, args string
		setup             func(*callServer)
		wantActs          int
	}{
		{"applied", "refresh_decypharr_links", `{}`, nil, 1},
		{
			"failed",
			"refresh_decypharr_links",
			`{}`,
			func(s *callServer) { s.repairRunCode = http.StatusInternalServerError },
			1,
		},
		{"refused", "arr_search_missing", searchEpisode7, func(s *callServer) { s.episodeHasFile = true }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			s, srv := newCallServer(t)
			if tc.setup != nil {
				tc.setup(s)
			}
			ag, database, inc := newRig(t, srv.URL, []string{
				toolCallResponse(tc.tool, tc.args),
				toolCallResponse(tc.tool, tc.args),
				toolCallResponse("complete_diagnosis", diagnosisArgs),
			}, rigOpts{})
			g := newRecGate(database, inc)
			exit := runGated(context.Background(), t, ag, database, inc, g)
			if exit.State != agent.RunConcluded {
				t.Fatalf("exit %s: %v", exit.State, exit.Err)
			}
			if got := g.acted(); len(got) != tc.wantActs {
				t.Errorf("Act ran %d times, want %d: %v", len(got), tc.wantActs, got)
			}
			if n := actionCount(t, database, inc.ID); n != tc.wantActs {
				t.Errorf("action_count = %d, want %d", n, tc.wantActs)
			}
			if n := len(g.Calls()); n != 1 {
				t.Errorf("%d gate records, want only the first call: %+v", n, g.Calls())
			}
		})
	}
}

func TestSeen_RefusedRepeatSaysRefused(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.episodeHasFile = true
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", searchEpisode7),
		toolCallResponse("arr_search_missing", searchEpisode7),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	replies := toolReplies(exit.Conversation)
	if len(replies) < 2 ||
		!strings.Contains(replies[1], "already refused in this run: "+agent.ErrArrTargetHasFile.Error()) {
		t.Errorf("replies = %v, want the repeat to say it was refused with the reason", replies)
	}
}

func TestSeen_NotRunDoesNotBlock(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	args := `{"media_type":"tv","title":"Rick and Morty","season":9,"episode":9}`
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", args),
		toolCallResponse("arr_search_missing", args),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{onLLM: func() { s.failSeries.Store(1) }})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 1 {
		t.Errorf("Act ran %d times, want once for the repeat of the not-run call: %v", len(got), got)
	}
	if n := len(g.Calls()); n != 1 {
		t.Errorf("%d gate records, want only the call that ran: %+v", n, g.Calls())
	}
}

func TestSeen_IdentityIgnoresExtraArgs(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.repairRunCode = http.StatusInternalServerError
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("refresh_decypharr_links", `{"reason":"x"}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if got := g.acted(); len(got) != 1 {
		t.Fatalf("Act ran %d times, want once: %v", len(got), got)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) < 2 || !strings.Contains(replies[1], "already tried in this run and it failed") {
		t.Errorf("replies = %v, want the extra-arg repeat blocked as a failed repeat", replies)
	}
}

func TestSeen_DifferentEpisodesNotBlocked(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.episodeHasFile = true
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", searchEpisode7),
		toolCallResponse("arr_search_missing", `{"media_type":"tv","title":"Rick and Morty","season":9,"episode":9}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 1 {
		t.Errorf("Act ran %d times, want the second episode to run: %v", len(got), got)
	}
}

func TestReviewP3_SeasonSearchAfterEpisodeNotBlocked(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.episodeHasFile = true
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("arr_search_missing", searchEpisode7),
		toolCallResponse("arr_search_missing", `{"media_type":"tv","title":"Rick and Morty","season":9}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 1 {
		t.Errorf("Act ran %d times, want the season search to run: %v", len(got), got)
	}
	if n := len(g.Calls()); n != 2 {
		t.Errorf("%d gate records, want the refused episode and the season search: %+v", n, g.Calls())
	}
}

func TestSeen_RetryAttemptInheritsCalls(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.repairRunCode = http.StatusInternalServerError
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	g := newRecGate(database, inc)
	if exit := ag.Run(context.Background(), inc, nil, g); exit.State != agent.RunConcluded {
		t.Fatalf("attempt 1 exit %s: %v", exit.State, exit.Err)
	}
	exit := ag.Run(context.Background(), inc, nil, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("attempt 2 exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 1 {
		t.Errorf("Act ran %d times, want the second attempt blocked: %v", len(got), got)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], "already tried in this run and it failed") {
		t.Errorf("attempt 2 replies = %v, want the inherited failure", replies)
	}
}

func TestSeen_RerunNewGenerationStartsEmpty(t *testing.T) {
	t.Parallel()
	s, srv := newCallServer(t)
	s.repairRunCode = http.StatusInternalServerError
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	first := newRecGate(database, inc)
	ag.Run(context.Background(), inc, nil, first)
	second := newRecGate(database, inc)
	if n := len(second.Calls()); n != 0 {
		t.Fatalf("new gate starts with %d records", n)
	}
	exit := ag.Run(context.Background(), inc, nil, second)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := second.acted(); len(got) != 1 {
		t.Errorf("Act ran %d times under the new gate, want once: %v", len(got), got)
	}
}

func TestSeen_InterruptedRepeatBlocked(t *testing.T) {
	t.Parallel()
	srv := hangingRepairRun(t)
	ag, database, inc := newRig(t, srv.URL, []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}, rigOpts{})
	ag.SetSideEffectBoundForTest(150 * time.Millisecond)
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 1 {
		t.Fatalf("Act ran %d times, want once: %v", len(got), got)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) < 2 || !strings.Contains(replies[1], "its outcome is unknown (interrupted)") {
		t.Errorf("replies = %v, want the repeat to say the outcome is unknown (interrupted)", replies)
	}
}

func TestL12_SeasonSearchNotBlockedByEpisodeSearch(t *testing.T) {
	t.Parallel()
	const season1 = `{"media_type":"tv","title":"Rick and Morty","season":1}`
	const episode3 = `{"media_type":"tv","title":"Rick and Morty","season":1,"episode":3}`
	logged := func(episode float64) map[string]any {
		return map[string]any{"media_type": "tv", "title": "Rick and Morty", "season": float64(1), "episode": episode}
	}
	cases := []struct {
		name     string
		applied  map[string]any
		proposed string
		want     bool
	}{
		{"episode applied, season proposed", logged(3), season1, false},
		{"season applied, episode proposed", logged(-1), episode3, true},
		{"season applied, season proposed", logged(-1), season1, true},
		{"episode applied, same episode proposed", logged(3), episode3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database := newTestAgentDB(t)
			ctx := context.Background()
			inc := &db.Incident{
				Status:     db.StatusReopened,
				Source:     "discord",
				ReportedBy: "x",
				What:       "cant_play",
				Title:      "Rick and Morty",
			}
			lifecycletest.Incident(t, database, inc)
			err := database.LogAction(ctx, &db.ActionLog{
				IncidentID: inc.ID, Action: "arr_search_missing", Params: tc.applied,
				TriggeredBy: "agent", Status: db.ActionApplied,
			})
			if err != nil {
				t.Fatal(err)
			}
			a := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler))
			if got := a.ActionAlreadyAppliedForTest(ctx, inc.ID, "arr_search_missing", tc.proposed); got != tc.want {
				t.Errorf("already applied = %v, want %v", got, tc.want)
			}
		})
	}
}
