package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

type finishedPayload struct {
	Rounds int    `json:"rounds"`
	Error  string `json:"error"`
	Exit   string `json:"exit"`
}

type runTrace struct {
	starts, finishes int
	roundsAtFinish   int
	last             finishedPayload
	kinds            []string
}

func traceRuns(t *testing.T, database *db.DB, id string) runTrace {
	t.Helper()
	events, err := database.EventsSince(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var tr runTrace
	rounds := 0
	for _, e := range events {
		tr.kinds = append(tr.kinds, e.Kind)
		switch e.Kind {
		case "run_started":
			tr.starts++
			rounds = 0
		case "llm_round":
			rounds++
		case "run_finished":
			tr.finishes++
			tr.roundsAtFinish = rounds
			if jerr := json.Unmarshal(e.Payload, &tr.last); jerr != nil {
				t.Fatal(jerr)
			}
		}
	}
	return tr
}

func checkJ1J2(t *testing.T, tr runTrace) {
	t.Helper()
	if tr.starts != 1 || tr.finishes != 1 {
		t.Errorf("J2: %d run_started and %d run_finished, want one each: %v", tr.starts, tr.finishes, tr.kinds)
	}
	if tr.last.Rounds != tr.roundsAtFinish {
		t.Errorf("J1: run_finished rounds = %d, but %d llm_round events", tr.last.Rounds, tr.roundsAtFinish)
	}
}

func textResponse(content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "c", "object": "chat.completion", "model": "stub",
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content},
		}},
	})
	return string(b)
}

func completeAt(round int) []string {
	out := make([]string, 0, round+1)
	for range round {
		out = append(out, toolCallResponse("get_disk_info", `{}`))
	}
	return append(out, toolCallResponse("complete_diagnosis", diagnosisArgs))
}

func failingLLM() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	})
}

type blockingLLM struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	before  func()
	body    string
}

func newBlockingLLM() *blockingLLM {
	return &blockingLLM{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingLLM) free() { close(b.release) }

func (b *blockingLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.once.Do(func() { close(b.started) })
	if b.before != nil {
		b.before()
	}
	if b.body != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(b.body))
		return
	}
	select {
	case <-r.Context().Done():
	case <-b.release:
	}
}

func seedOldFinding(t *testing.T, database *db.DB, id string) {
	t.Helper()
	old := agent.DiagnosticResult{
		RootCause: "old run", PrimaryAction: "x", RequiresApproval: true,
		EscalateAction: agent.EscalateRemoveAndSearch,
	}
	if err := database.SetIncidentFinding(context.Background(), id, old, old); err != nil {
		t.Fatal(err)
	}
}

func storedFinding(t *testing.T, database *db.DB, id string) *agent.DiagnosticResult {
	t.Helper()
	inc, err := database.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := agent.ParseDiagnosticResult(inc.Finding)
	if !ok {
		t.Fatalf("no stored finding: %v", inc.Finding)
	}
	return f
}

func checkJ4(t *testing.T, database *db.DB, id, rootPrefix string) {
	t.Helper()
	f := storedFinding(t, database, id)
	if f.EscalateAction != agent.EscalateManualInvestigation || !f.RequiresApproval {
		t.Errorf(
			"J4: stored escalate_action = %q (approval %v), want manual_investigation",
			f.EscalateAction,
			f.RequiresApproval,
		)
	}
	if !strings.HasPrefix(f.RootCause, rootPrefix) {
		t.Errorf("stored root cause = %q, want prefix %q", f.RootCause, rootPrefix)
	}
}

func checkJ5(t *testing.T, database *db.DB, id string) {
	t.Helper()
	if f := storedFinding(
		t,
		database,
		id,
	); f.RootCause != "old run" ||
		f.EscalateAction != agent.EscalateRemoveAndSearch {
		t.Errorf("J5: stored finding changed to %+v", f)
	}
}

func spendBudget(t *testing.T, database *db.DB, id string) {
	t.Helper()
	for range 3 {
		if _, err := database.IncrementActionCount(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestA3_5_TextOnlyReplyExitsStoppedNoTools(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{textResponse("I think it is fine.")}})
	inc := seedRunIncident(t, database)

	result, _, err := runOnce(context.Background(), ag, inc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := traceRuns(t, database, inc.ID)
	checkJ1J2(t, tr)
	if tr.last.Exit != "stopped_no_tools" || tr.last.Rounds != 1 || tr.last.Error != "" {
		t.Errorf("run_finished = %+v, want stopped_no_tools after 1 round with no error", tr.last)
	}
	if result == nil || result.RootCause != "the agent stopped without calling complete_diagnosis" {
		t.Errorf("result = %+v, want the stopped-without-diagnosis finding", result)
	}
}

func TestA4_4_RunFinishedRoundsMatchLLMRounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		llm    http.Handler
		spent  bool
		exit   string
		rounds int
	}{
		{"concluded at round 0", &scriptedLLM{responses: completeAt(0)}, false, "concluded", 1},
		{"concluded at round 2", &scriptedLLM{responses: completeAt(2)}, false, "concluded", 3},
		{"text only", &scriptedLLM{responses: []string{textResponse("done")}}, false, "stopped_no_tools", 1},
		{"budget", &scriptedLLM{responses: []string{
			toolCallResponse("clear_jellyfin_cache", `{"item_id":"abc"}`),
		}}, true, "budget_tripped", 1},
		{"exhausted", &scriptedLLM{responses: []string{
			toolCallResponse("get_disk_info", `{}`),
		}}, false, "exhausted", agent.MaxRoundsForTest},
		{"llm 500", failingLLM(), false, "llm_failed", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ag, database := newRunTestAgent(t, c.llm)
			inc := seedRunIncident(t, database)
			if c.spent {
				spendBudget(t, database, inc.ID)
			}
			_, _, _ = runOnce(context.Background(), ag, inc)
			tr := traceRuns(t, database, inc.ID)
			checkJ1J2(t, tr)
			if tr.last.Exit != c.exit || tr.last.Rounds != c.rounds {
				t.Errorf("run_finished = %+v, want exit %s after %d rounds", tr.last, c.exit, c.rounds)
			}
		})
	}
}

func TestA4_8_SupersededRunStillRecordsRunFinished(t *testing.T) {
	t.Parallel()
	llm := newBlockingLLM()
	defer llm.free()
	ag, database := newRunTestAgent(t, llm)
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-llm.started
		cancel()
	}()

	_, _, err := runOnce(ctx, ag, inc)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want it to wrap context.Canceled", err)
	}
	tr := traceRuns(t, database, inc.ID)
	checkJ1J2(t, tr)
	if tr.last.Exit != "superseded" {
		t.Errorf("run_finished exit = %q, want superseded", tr.last.Exit)
	}
	checkJ5(t, database, inc.ID)
}

func TestA3_8_BudgetTripReplacesOldFinding(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{
		toolCallResponse("clear_jellyfin_cache", `{"item_id":"abc"}`),
	}})
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)
	spendBudget(t, database, inc.ID)

	if _, _, err := runOnce(context.Background(), ag, inc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	checkJ4(t, database, inc.ID, "max autonomous actions reached without resolution")
}

func TestA3_8_LLMFailureReplacesOldFinding(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, failingLLM())
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)

	if _, _, err := runOnce(context.Background(), ag, inc); err == nil {
		t.Fatal("Run succeeded against a failing LLM")
	}
	checkJ4(t, database, inc.ID, "the LLM call failed")
	checkJ1J2(t, traceRuns(t, database, inc.ID))
}

func TestA3_8_CancelAroundReplyEndsSupersededWithoutFinding(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	llm := newBlockingLLM()
	defer llm.free()
	llm.before = cancel
	llm.body = textResponse("all good")
	ag, database := newRunTestAgent(t, llm)
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)

	_, _, _ = runOnce(ctx, ag, inc)
	tr := traceRuns(t, database, inc.ID)
	checkJ1J2(t, tr)
	if tr.last.Exit != "superseded" {
		t.Errorf("run_finished exit = %q, want superseded", tr.last.Exit)
	}
	checkJ5(t, database, inc.ID)
}

func TestA3_11_ExhaustedReplacesOldFinding(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{toolCallResponse("get_disk_info", `{}`)}})
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)

	if _, _, err := runOnce(context.Background(), ag, inc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	checkJ4(t, database, inc.ID, "diagnostic loop exhausted without conclusion")
}

func TestA7_8_SyntheticExitFindingsArePersisted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		resp  string
		spent bool
	}{
		{"budget", toolCallResponse("clear_jellyfin_cache", `{"item_id":"abc"}`), true},
		{"exhausted", toolCallResponse("get_disk_info", `{}`), false},
		{"text only", textResponse("hm"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{c.resp}})
			inc := seedRunIncident(t, database)
			if c.spent {
				spendBudget(t, database, inc.ID)
			}
			result, _, err := runOnce(context.Background(), ag, inc)
			if err != nil || result == nil {
				t.Fatalf("Run = %+v, %v", result, err)
			}
			stored := storedFinding(t, database, inc.ID)
			if stored.RootCause != result.RootCause || stored.EscalateAction != result.EscalateAction {
				t.Errorf("stored %+v, returned %+v, want the same finding", stored, result)
			}
		})
	}
}

func TestRun_NotClaimedRecordsNothing(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{textResponse("x")}})
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)
	if err := database.UpdateIncidentStatus(context.Background(), inc.ID, db.StatusResolved); err != nil {
		t.Fatal(err)
	}

	_, _, err := runOnce(context.Background(), ag, inc)
	if !errors.Is(err, agent.ErrIncidentNotInvestigatable) {
		t.Errorf("Run error = %v, want ErrIncidentNotInvestigatable", err)
	}
	if tr := traceRuns(t, database, inc.ID); len(tr.kinds) != 0 {
		t.Errorf("events = %v, want none", tr.kinds)
	}
	checkJ5(t, database, inc.ID)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestLLMCall_DoneContextReturnsWithoutRetryWarn(t *testing.T) {
	t.Parallel()
	llm := newBlockingLLM()
	defer llm.free()
	logs := &syncBuffer{}
	ag, database := newLoggedRunTestAgent(t, llm, slog.New(slog.NewTextHandler(logs, nil)))
	inc := seedRunIncident(t, database)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-llm.started
		cancel()
	}()

	_, _, _ = runOnce(ctx, ag, inc)
	if strings.Contains(logs.String(), "llm transient error, retrying") {
		t.Errorf("a cancelled run logged a retry warning:\n%s", logs.String())
	}
}

func runOnce(
	ctx context.Context, ag *agent.Agent, inc *db.Incident,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	exit := ag.Run(ctx, inc, nil)
	return exit.Finding, exit.Conversation, exit.Err
}

func TestA3_8_ConcludedExitKeepsItsFinding(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: completeAt(1)})
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)

	if _, _, err := runOnce(context.Background(), ag, inc); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr := traceRuns(t, database, inc.ID)
	checkJ1J2(t, tr)
	if tr.last.Exit != "concluded" {
		t.Fatalf("exit = %q, want concluded", tr.last.Exit)
	}
	if f := storedFinding(t, database, inc.ID); f.RootCause != "x" || f.PrimaryAction != "decypharr_recheck" {
		t.Errorf("stored finding after run_finished = %+v, want the concluded diagnosis", f)
	}
}
