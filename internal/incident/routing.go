package incident

import (
	"context"
	"errors"
	"fmt"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func (s *Service) runAgent(
	ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage, t Timers, gate *runGate,
) {
	if s.agent == nil {
		return
	}
	retryDelays := []time.Duration{0, s.minutes(t.RetryFirstMinutes), s.minutes(t.RetrySecondMinutes)}
	for attempt, delay := range retryDelays {
		if attempt > 0 {
			if s.stopBeforeRetry(ctx, inc, gate) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if s.stopBeforeRetry(ctx, inc, gate) {
				return
			}
		}
		exit := s.runDiagnosis(ctx, inc, seed, gate)
		outcome := s.routeExit(ctx, inc, gate.gen, exit, attempt, len(retryDelays))
		if outcome.done {
			return
		}
		seed = outcome.seed
	}
}

func (s *Service) stopBeforeRetry(ctx context.Context, inc *db.Incident, gate *runGate) bool {
	switch gate.Live(ctx) {
	case agent.GatePaused:
		s.park(ctx, inc, gate.gen)
		return true
	case agent.GateSuperseded:
		s.log.InfoContext(ctx, "retry dropped, incident moved on", "incident", inc.ID)
		return true
	case agent.GateAllowed, agent.GateTripped, agent.GateDiagnosisOnly:
	}
	return false
}

func (s *Service) park(ctx context.Context, inc *db.Incident, gen int64) {
	s.applyOutcome(ctx, inc.ID, lifecycle.Event{
		Kind: lifecycle.RunFinished, Gen: gen, Outcome: lifecycle.Outcome{Kind: lifecycle.OutcomeParked},
	})
}

func (s *Service) routeExit(
	ctx context.Context, inc *db.Incident, gen int64, exit agent.RunExit, attempt, maxAttempts int,
) diagnosisOutcome {
	switch exit.State {
	case agent.RunConcluded, agent.RunStoppedNoTools, agent.RunExhausted:
		if exit.Finding != nil {
			return s.evaluateDiagnosis(ctx, inc, gen, exit.Finding, exit.Conversation, attempt, maxAttempts)
		}
		s.handleAgentError(ctx, inc, gen, errRunNotExited)
	case agent.RunBudgetTripped:
		s.budgetSpent(ctx, inc, gen, exit.Finding)
	case agent.RunLLMFailed, agent.RunBatchFailed:
		s.handleAgentError(ctx, inc, gen, exit.Err)
	case agent.RunClaimFailed:
		s.log.ErrorContext(ctx, "run could not claim the incident, left for the reconciler",
			"incident", inc.ID, "error", exit.Err)
	case agent.RunNotClaimed:
		if errors.Is(exit.Err, lifecycle.ErrPaused) {
			s.park(ctx, inc, gen)
			break
		}
		s.log.InfoContext(ctx, "run superseded, exiting quietly", "incident", inc.ID, "exit", string(exit.State))
	case agent.RunSuperseded:
		s.log.InfoContext(ctx, "run superseded, exiting quietly", "incident", inc.ID, "exit", string(exit.State))
	case agent.RunPaused:
		s.park(ctx, inc, gen)
	case agent.RunClaiming, agent.RunAwaitingReply, agent.RunInBatch:
		s.log.ErrorContext(ctx, "agent run returned without exiting", "incident", inc.ID, "state", string(exit.State))
		s.handleAgentError(ctx, inc, gen, errRunNotExited)
	}
	return diagnosisOutcome{done: true}
}

func (s *Service) handleAgentResolved(
	ctx context.Context, inc *db.Incident, gen int64, result *agent.DiagnosticResult,
) {
	if inc.What == agent.WhatStuckRead {
		s.runVerification(ctx, inc, gen, result)
		return
	}
	if result.PrimaryAction == agent.ToolArrSearchMissing {
		s.startPendingOutcomeTracking(ctx, inc, gen, result)
		return
	}
	if result.VerifyAfterSeconds > 0 {
		s.runVerification(ctx, inc, gen, result)
		return
	}
	s.log.InfoContext(ctx, "agent fixed", "incident", inc.ID, "action", result.PrimaryAction)
	s.applyOutcome(ctx, inc.ID, lifecycle.Event{
		Kind: lifecycle.RunFinished, Gen: gen, Outcome: lifecycle.Outcome{Kind: lifecycle.OutcomeFixed},
	})
}

func (s *Service) escalate(ctx context.Context, inc *db.Incident, gen int64, reason lifecycle.Reason, c card.Card) {
	s.applyOutcome(ctx, inc.ID, lifecycle.Event{
		Kind: lifecycle.RunFinished, Gen: gen, Notice: noticeOf(c),
		Outcome: lifecycle.Outcome{Kind: lifecycle.OutcomeEscalate, Reason: reason},
	})
}

func (s *Service) handleAgentError(ctx context.Context, inc *db.Incident, gen int64, err error) {
	s.log.ErrorContext(ctx, "agent error", "incident", inc.ID, "error", err)
	s.escalate(ctx, inc, gen, lifecycle.ReasonAgentError, ownerCard(inc, card.Warn, fmt.Sprintf(
		"❌ Agent error for incident **%s** (#%s): %v", inc.Title, inc.ShortID(), err,
	), "Incident marked for manual review."))
}

func surfaceCard(inc *db.Incident, result *agent.DiagnosticResult, note string) card.Card {
	return ownerCard(inc, card.Warn, fmt.Sprintf(
		"🔍 Incident **%s** (#%s) needs your attention%s.", inc.Title, inc.ShortID(), note,
	), "",
		card.Field{Name: fieldRootCause, Value: result.RootCause},
		card.Field{Name: fieldAction, Value: agent.EscalationSummary(result)},
		card.Field{Name: fieldReason, Value: result.PrimaryReason})
}

func (s *Service) surfaceToOwner(
	ctx context.Context, inc *db.Incident, gen int64, result *agent.DiagnosticResult, note string,
) {
	s.escalate(ctx, inc, gen, lifecycle.ReasonNeedsOwner, surfaceCard(inc, result, note))
}

func (s *Service) budgetSpent(ctx context.Context, inc *db.Incident, gen int64, result *agent.DiagnosticResult) {
	if result == nil {
		result = &agent.DiagnosticResult{RootCause: agent.RunMeta(agent.RunBudgetTripped).Reason}
	}
	s.escalate(ctx, inc, gen, lifecycle.ReasonBudgetSpent, surfaceCard(inc, result, ""))
}
