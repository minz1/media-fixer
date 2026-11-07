package agent_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
)

const unknownMark = "(outcome unknown (interrupted)"

const unknownReply = "not run: an earlier restart_jellyfin attempt was interrupted and its outcome is unknown; " +
	"check the service or escalate"

func seedAttempt(t *testing.T, d *db.DB, id string, gen int64, tool string, params map[string]any) {
	t.Helper()
	ev, err := journal.ActionAttemptedEvent(id, gen, tool, params)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.AppendEvents(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func seedOwnerRerun(t *testing.T, d *db.DB, id string, gen int64) {
	t.Helper()
	ev, err := journal.OwnerRerunEvent(id, gen)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.AppendEvents(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func seedApplied(t *testing.T, d *db.DB, id string, gen int64, tool string, params map[string]any) {
	t.Helper()
	err := journal.New(d).LogActionGen(context.Background(), &db.ActionLog{
		IncidentID: id, Action: tool, Params: params, TriggeredBy: "agent", Status: db.ActionApplied,
	}, false, gen)
	if err != nil {
		t.Fatal(err)
	}
}

func closedDB(t *testing.T) *db.DB {
	t.Helper()
	d := newDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestL1_InterruptedDisruptiveAttemptBlocksRepeat(t *testing.T) {
	t.Parallel()
	for _, gen := range []int64{4, 5} {
		t.Run("", func(t *testing.T) {
			t.Parallel()
			_, srv := newCallServer(t)
			ag, database, inc := newRig(t, srv.URL, script("restart_jellyfin", `{}`), rigOpts{})
			seedAttempt(t, database, inc.ID, 3, "restart_jellyfin", nil)
			lifecycletest.Force(t, database, inc.ID, func(s *lifecycle.Snapshot) { s.Generation = gen })
			g := newRecGate(database, inc)
			g.gen = gen
			exit := ag.Run(context.Background(), inc, nil, g)
			if got := g.acted(); len(got) != 0 {
				t.Errorf("Act ran %v, want none", got)
			}
			replies := toolReplies(exit.Conversation)
			if len(replies) == 0 || !strings.Contains(replies[0], unknownReply) {
				t.Errorf("replies = %v, want %q", replies, unknownReply)
			}
			if got := ag.AttemptHistoryForTest(context.Background(), inc.ID); !strings.Contains(got, unknownMark) {
				t.Errorf("history missing the unknown line:\n%s", got)
			}
		})
	}
}

func TestL1_OwnerRerunLiftsInterruptedBlock(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, script("restart_jellyfin", `{}`), rigOpts{})
	seedAttempt(t, database, inc.ID, inc.Generation-1, "restart_jellyfin", nil)
	seedOwnerRerun(t, database, inc.ID, inc.Generation)
	g := newRecGate(database, inc)
	ag.Run(context.Background(), inc, nil, g)
	if got := g.acted(); len(got) != 1 {
		t.Errorf("Act ran %v, want once", got)
	}
	if got := ag.AttemptHistoryForTest(context.Background(), inc.ID); !strings.Contains(got, unknownMark) {
		t.Errorf("history lost the lifted attempt's line:\n%s", got)
	}
}

func TestL1_NonDisruptiveInterruptedAllowedAndLabelled(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	ag, database, inc := newRig(t, srv.URL, script("clear_jellyfin_cache", `{}`), rigOpts{})
	seedAttempt(t, database, inc.ID, inc.Generation-1, "clear_jellyfin_cache", nil)
	g := newRecGate(database, inc)
	ag.Run(context.Background(), inc, nil, g)
	if got := g.acted(); len(got) != 1 {
		t.Errorf("Act ran %v, want once", got)
	}
	if got := ag.AttemptHistoryForTest(context.Background(), inc.ID); !strings.Contains(got, unknownMark) {
		t.Errorf("history missing the unknown line:\n%s", got)
	}
}

func TestL1_PairingNeedsSameGenerationToolAndIdentity(t *testing.T) {
	t.Parallel()
	search := map[string]any{"title": "Show", "season": 1, "episode": 2}
	cases := []struct {
		name    string
		gen     int64
		tool    string
		params  map[string]any
		unknown int
	}{
		{"other generation", 4, "arr_search_missing", search, 1},
		{"other tool", 3, "sonarr_rescan", search, 1},
		{"other episode", 3, "arr_search_missing", map[string]any{"title": "Show", "season": 1, "episode": 3}, 1},
		{"matching", 3, "arr_search_missing", search, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			database := newTestAgentDB(t)
			inc := seedRunIncident(t, database)
			seedAttempt(t, database, inc.ID, 3, "arr_search_missing", search)
			seedApplied(t, database, inc.ID, tc.gen, tc.tool, tc.params)
			a := agent.New(nil, "", nil, database, journal.New(database), slog.New(slog.DiscardHandler))
			got := a.AttemptHistoryForTest(context.Background(), inc.ID)
			if n := strings.Count(got, unknownMark); n != tc.unknown {
				t.Errorf("unknown lines = %d, want %d:\n%s", n, tc.unknown, got)
			}
		})
	}
}

type attemptGate struct {
	*recGate

	d *db.DB
}

func (g attemptGate) Act(ctx context.Context, tool string, params map[string]any) (agent.Verdict, error) {
	v, err := g.recGate.Act(ctx, tool, params)
	if err == nil && v == agent.GateAllowed {
		ev, _ := journal.ActionAttemptedEvent(g.id, g.gen, tool, params)
		_ = g.d.AppendEvents(ctx, ev)
	}
	return v, err
}

func agentOver(t *testing.T, database *db.DB, srvURL string, j *journal.Journal) *agent.Agent {
	t.Helper()
	llm := &scriptedLLM{responses: script("restart_jellyfin", `{}`)}
	llmSrv := httptest.NewServer(http.HandlerFunc(llm.ServeHTTP))
	t.Cleanup(llmSrv.Close)
	cfg := openai.DefaultConfig("stub-key")
	cfg.BaseURL = llmSrv.URL
	return agent.New(openai.NewClientWithConfig(cfg), "stub", fullDispatcher(srvURL), database, j,
		slog.New(slog.DiscardHandler))
}

func TestL1_SigtermAfterAttemptBeforeRowIsUnknown(t *testing.T) {
	t.Parallel()
	_, srv := newCallServer(t)
	_, database, inc := newRig(t, srv.URL, nil, rigOpts{})
	first := agentOver(t, database, srv.URL, nil)
	first.Run(context.Background(), inc, nil, attemptGate{recGate: newRecGate(database, inc), d: database})
	if acts := listActions(t, database, inc.ID); len(acts) != 0 {
		t.Fatalf("actions = %+v, want the record write dropped", acts)
	}

	next := agentOver(t, database, srv.URL, journal.New(database))
	lifecycletest.Force(t, database, inc.ID, func(s *lifecycle.Snapshot) {
		s.Generation, s.Status = inc.Generation+1, lifecycle.Open
	})
	g2 := newRecGate(database, inc)
	g2.gen = inc.Generation + 1
	exit := next.Run(context.Background(), inc, nil, g2)
	if got := g2.acted(); len(got) != 0 {
		t.Errorf("Act ran %v in the next generation, want none", got)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], unknownReply) {
		t.Errorf("replies = %v, want %q", replies, unknownReply)
	}
}

func TestL1_HistoryReadErrorBlocksDisruptive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool    string
		blocked bool
	}{{"restart_jellyfin", true}, {"clear_jellyfin_cache", false}} {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			_, srv := newCallServer(t)
			ag, database, inc := newRig(t, srv.URL, script(tc.tool, `{}`), rigOpts{journalDB: closedDB(t)})
			g := newRecGate(database, inc)
			exit := ag.Run(context.Background(), inc, nil, g)
			acted := len(g.acted()) == 1
			if acted == tc.blocked {
				t.Errorf("acted = %v, blocked want %v", acted, tc.blocked)
			}
			replies := toolReplies(exit.Conversation)
			if tc.blocked && (len(replies) == 0 || !strings.Contains(replies[0], "not run:")) {
				t.Errorf("replies = %v, want a not-run reply", replies)
			}
		})
	}
}
