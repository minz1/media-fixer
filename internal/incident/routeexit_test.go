package incident_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

func exitOf(result *agent.DiagnosticResult, conv []openai.ChatCompletionMessage, err error) agent.RunExit {
	switch {
	case errors.Is(err, context.Canceled):
		return agent.RunExit{State: agent.RunSuperseded, Conversation: conv, Err: err}
	case errors.Is(err, agent.ErrIncidentNotInvestigatable):
		return agent.RunExit{State: agent.RunNotClaimed, Err: err}
	case err != nil:
		return agent.RunExit{State: agent.RunLLMFailed, Conversation: conv, Err: err}
	case result == nil:
		return agent.RunExit{State: agent.RunSuperseded, Conversation: conv, Err: context.Canceled}
	}
	return agent.RunExit{State: agent.RunConcluded, Finding: result, Conversation: conv}
}

type fixedExitAgent struct {
	*escalationAgent

	exit agent.RunExit
}

func (a *fixedExitAgent) Run(context.Context, *db.Incident, []openai.ChatCompletionMessage) agent.RunExit {
	return a.exit
}

func allRunStates() []agent.RunState {
	return []agent.RunState{
		agent.RunClaiming, agent.RunAwaitingReply, agent.RunInBatch, agent.RunConcluded, agent.RunBudgetTripped,
		agent.RunStoppedNoTools, agent.RunExhausted, agent.RunLLMFailed, agent.RunBatchFailed, agent.RunSuperseded,
		agent.RunNotClaimed, agent.RunClaimFailed,
	}
}

func manualExit(s agent.RunState) agent.RunExit {
	return agent.RunExit{State: s, Rounds: 1, Err: errors.New("boom"), Finding: &agent.DiagnosticResult{
		RootCause: agent.RunMeta(s).Reason, Confidence: "low", PrimaryAction: agent.EscalateManualInvestigation,
		EscalateAction: agent.EscalateManualInvestigation, RequiresApproval: true,
	}}
}

func routeFixture(t *testing.T) (*escalationFixture, *captureNotifier, *db.Incident) {
	t.Helper()
	notif := &captureNotifier{}
	fx := newEscalationFixtureNotifier(t, newEscalationAgent(), notif)
	inc := &db.Incident{Status: db.StatusInvestigating, Source: "discord", What: "cant_play", Title: "The Boys"}
	if err := fx.db.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return fx, notif, inc
}

func status(t *testing.T, d *db.DB, id string) db.IncidentStatus {
	t.Helper()
	got, err := d.GetIncident(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

func TestRouteExit_EveryStateHasARoute(t *testing.T) {
	t.Parallel()
	quiet := []agent.RunState{agent.RunSuperseded, agent.RunNotClaimed}
	for _, s := range allRunStates() {
		t.Run(string(s), func(t *testing.T) {
			t.Parallel()
			fx, notif, inc := routeFixture(t)
			if !fx.svc.RouteExitForTest(context.Background(), inc, manualExit(s)) {
				t.Fatalf("%s left the retry loop running", s)
			}
			if slices.Contains(quiet, s) != (len(notif.msgs) == 0) {
				t.Errorf("%s sent %v to the owner", s, notif.msgs)
			}
		})
	}
}

func TestRouteExit_SupersededIsQuiet(t *testing.T) {
	t.Parallel()
	for _, s := range []agent.RunState{agent.RunSuperseded, agent.RunNotClaimed} {
		fx, notif, inc := routeFixture(t)
		fx.svc.RouteExitForTest(context.Background(), inc, agent.RunExit{State: s, Err: context.Canceled})
		if len(notif.msgs) != 0 {
			t.Errorf("%s notified %v", s, notif.msgs)
		}
		if got := status(t, fx.db, inc.ID); got != db.StatusInvestigating {
			t.Errorf("%s moved the incident to %s", s, got)
		}
	}
}

func TestRouteExit_ClaimFailedEscalates(t *testing.T) {
	t.Parallel()
	fx, notif, inc := routeFixture(t)
	fx.svc.RouteExitForTest(context.Background(), inc, agent.RunExit{
		State: agent.RunClaimFailed, Err: errors.New("database is locked"),
	})
	if len(notif.msgs) != 1 || !strings.Contains(notif.msgs[0], "database is locked") {
		t.Fatalf("owner messages = %v, want the claim error", notif.msgs)
	}
	if got := status(t, fx.db, inc.ID); got != db.StatusManualTestNeeded {
		t.Errorf("status = %s, want manual_test_needed", got)
	}
}

func TestRunAgent_BudgetTrippedExitReachesOwner(t *testing.T) {
	t.Parallel()
	notif := &captureNotifier{}
	ag := &fixedExitAgent{escalationAgent: newEscalationAgent(), exit: manualExit(agent.RunBudgetTripped)}
	fx := newEscalationFixtureNotifier(t, ag, notif)
	inc := &db.Incident{Status: db.StatusInvestigating, Source: "discord", What: "cant_play", Title: "The Boys"}
	if err := fx.db.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	fx.svc.RunAgentForTest(context.Background(), inc)
	if len(notif.msgs) != 1 || !strings.Contains(notif.msgs[0], "needs your attention") {
		t.Fatalf("owner messages = %v, want the budget exit surfaced", notif.msgs)
	}
	if got := status(t, fx.db, inc.ID); got != db.StatusManualTestNeeded {
		t.Errorf("status = %s, want manual_test_needed", got)
	}
}
