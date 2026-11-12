package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

type recGate struct {
	d      *db.DB
	id     string
	gen    int64
	paused atomic.Bool
	stale  atomic.Bool
	mu     sync.Mutex
	acts   []string
	calls  []agent.CallRecord
}

func newRecGate(d *db.DB, inc *db.Incident) *recGate {
	return &recGate{d: d, id: inc.ID, gen: inc.Generation}
}

func (g *recGate) apply(ctx context.Context, ev lifecycle.Event) (db.Applied, error) {
	ev.Gen = g.gen
	return g.d.ApplyIncident(ctx, g.id, lifecycletest.Decide(ev, lifecycletest.Limits(), time.Now()))
}

func (g *recGate) Claim(ctx context.Context) error {
	_, err := g.apply(ctx, lifecycle.Event{Kind: lifecycle.RunClaimed})
	if errors.Is(err, lifecycle.ErrStale) {
		return fmt.Errorf("%w: %w", agent.ErrIncidentNotInvestigatable, err)
	}
	return err
}

func (g *recGate) Act(ctx context.Context, tool string, _ map[string]any) (agent.Verdict, error) {
	g.mu.Lock()
	g.acts = append(g.acts, tool)
	g.mu.Unlock()
	a, err := g.apply(ctx, lifecycle.Event{Kind: lifecycle.ActionAttempted, Tool: tool})
	switch {
	case err == nil && a.Snap.Locked:
		return agent.GateTripped, nil
	case err == nil:
		return agent.GateAllowed, nil
	case errors.Is(err, lifecycle.ErrDiagnosisOnly):
		return agent.GateDiagnosisOnly, nil
	}
	return "", err
}

func (g *recGate) Gen() int64 { return g.gen }

func (g *recGate) ActionLogged(context.Context) {}

func (g *recGate) Live(context.Context) agent.Verdict {
	switch {
	case g.paused.Load():
		return agent.GatePaused
	case g.stale.Load():
		return agent.GateSuperseded
	}
	return agent.GateAllowed
}

func (g *recGate) Calls() []agent.CallRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]agent.CallRecord(nil), g.calls...)
}

func (g *recGate) Record(rec agent.CallRecord) {
	g.mu.Lock()
	g.calls = append(g.calls, rec)
	g.mu.Unlock()
}

func (g *recGate) acted() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.acts...)
}

func recordAttempt(t *testing.T, d *db.DB, g *recGate, exit agent.RunExit) {
	t.Helper()
	if !exit.Started {
		return
	}
	ev := lifecycle.Event{Kind: lifecycle.AttemptFinished, Gen: g.gen, Exit: string(exit.State)}
	if exit.Finding != nil && (exit.State == agent.RunConcluded || agent.RunMeta(exit.State).WritesFinding) {
		b, _ := json.Marshal(exit.Finding)
		ev.Finding = &lifecycle.Finding{EscalateAction: exit.Finding.EscalateAction, JSON: b}
	}
	decide := func(cur lifecycle.Snapshot) (db.Decision, error) {
		dec, err := lifecycletest.Decide(ev, lifecycletest.Limits(), time.Now())(cur)
		if exit.Journal != nil {
			dec.Events = []*db.Event{exit.Journal}
		}
		return dec, err
	}
	if _, err := d.ApplyIncident(context.Background(), g.id, decide); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
}

func runGated(
	ctx context.Context, t *testing.T, ag *agent.Agent, d *db.DB, inc *db.Incident, g *recGate,
) agent.RunExit {
	t.Helper()
	exit := ag.Run(ctx, inc, nil, g)
	recordAttempt(t, d, g, exit)
	return exit
}

func toolReplies(conv []openai.ChatCompletionMessage) []string {
	var out []string
	for _, m := range conv {
		if m.Role == openai.ChatMessageRoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestA1_6_BudgetCountedInApply(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("decypharr_repair_sweep", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}})
	inc := seedRunIncident(t, database)
	g := newRecGate(database, inc)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	if got := g.acted(); len(got) != 2 {
		t.Fatalf("gate saw %v, want both autonomous actions", got)
	}
	s, err := database.GetIncidentSnapshot(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.ActionCount != 2 {
		t.Fatalf("action_count %d, want 2 counted by the Apply", s.ActionCount)
	}
}

func TestRun_LockedRunsDiagnosisOnly(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}})
	inc := seedRunIncident(t, database)
	lifecycletest.Lock(t, database, inc.ID, true)
	exit := runGated(context.Background(), t, ag, database, inc, newRecGate(database, inc))
	if exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	replies := toolReplies(exit.Conversation)
	if len(replies) == 0 || !strings.Contains(replies[0], "not run: budget spent, diagnosis only") {
		t.Fatalf("first tool reply %v", replies)
	}
	if actions, _ := database.ListActions(context.Background(), inc.ID); len(actions) != 0 {
		t.Fatalf("a locked run dispatched %d actions", len(actions))
	}
}

func TestRun_GateStaleEndsSuperseded(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{toolCallResponse("get_disk_info", `{}`)}})
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)
	g := newRecGate(database, inc)
	g.stale.Store(true)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunSuperseded || exit.Finding != nil {
		t.Fatalf("exit %s finding %+v", exit.State, exit.Finding)
	}
	checkJ1J2(t, traceRuns(t, database, inc.ID))
	checkJ5(t, database, inc.ID)
}

func TestRun_GatePausedEndsPaused(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{toolCallResponse("get_disk_info", `{}`)}})
	inc := seedRunIncident(t, database)
	g := newRecGate(database, inc)
	g.paused.Store(true)
	exit := runGated(context.Background(), t, ag, database, inc, g)
	if exit.State != agent.RunPaused || exit.Finding != nil || !exit.Started {
		t.Fatalf("exit %s finding %+v started %v", exit.State, exit.Finding, exit.Started)
	}
	if tr := traceRuns(t, database, inc.ID); tr.last.Exit != "paused" {
		t.Fatalf("run_finished exit %q", tr.last.Exit)
	}
}

func TestRun_FinishWritesNothing(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{textResponse("hm")}})
	inc := seedRunIncident(t, database)
	seedOldFinding(t, database, inc.ID)
	exit := ag.Run(context.Background(), inc, nil, newRecGate(database, inc))
	if exit.State != agent.RunStoppedNoTools {
		t.Fatalf("exit %s", exit.State)
	}
	if tr := traceRuns(t, database, inc.ID); tr.finishes != 0 {
		t.Fatalf("the run appended run_finished itself: %v", tr.kinds)
	}
	checkJ5(t, database, inc.ID)
}

func TestRun_ExitCarriesRunFinishedEvent(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: completeAt(1)})
	inc := seedRunIncident(t, database)
	exit := ag.Run(context.Background(), inc, nil, newRecGate(database, inc))
	e := exit.Journal
	if e == nil || e.Kind != "run_finished" || e.Seq != 0 || e.IncidentID != inc.ID {
		t.Fatalf("exit journal %+v", e)
	}
	var p finishedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.Exit != "concluded" || p.Rounds != 2 {
		t.Fatalf("payload %s %v", e.Payload, err)
	}
}

func TestRecordCall_PayloadCarriesGeneration(t *testing.T) {
	t.Parallel()
	ag, database := newRunTestAgent(t, &scriptedLLM{responses: []string{
		toolCallResponse("refresh_decypharr_links", `{}`),
		toolCallResponse("complete_diagnosis", diagnosisArgs),
	}})
	inc := seedRunIncident(t, database)
	g := newRecGate(database, inc)
	if exit := runGated(context.Background(), t, ag, database, inc, g); exit.State != agent.RunConcluded {
		t.Fatalf("exit %s: %v", exit.State, exit.Err)
	}
	events, err := database.EventsSince(context.Background(), inc.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, e := range events {
		if e.Kind != "action_applied" {
			continue
		}
		seen++
		var p struct {
			Generation int64 `json:"generation"`
		}
		if err = json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Generation == 0 || p.Generation != g.gen {
			t.Errorf("action_applied generation %d, want the gate's %d: %s", p.Generation, g.gen, e.Payload)
		}
	}
	if seen != 1 {
		t.Fatalf("%d action_applied events, want 1", seen)
	}
}
