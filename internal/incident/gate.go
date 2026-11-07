package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

const attemptWriteTimeout = 5 * time.Second

type runGate struct {
	s   *Service
	id  string
	gen int64
}

func (s *Service) gateFor(id string, gen int64) *runGate { return &runGate{s: s, id: id, gen: gen} }

func (g *runGate) Claim(ctx context.Context) error {
	_, err := g.s.apply(ctx, g.id, lifecycle.Event{Kind: lifecycle.RunClaimed, Gen: g.gen}, nil)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, lifecycle.ErrStale), errors.Is(err, lifecycle.ErrParked), errors.Is(err, lifecycle.ErrPaused):
		return fmt.Errorf("%w: %w", agent.ErrIncidentNotInvestigatable, err)
	}
	return err
}

func (g *runGate) Act(ctx context.Context, tool string) (agent.Verdict, error) {
	a, err := g.s.apply(ctx, g.id, lifecycle.Event{Kind: lifecycle.ActionAttempted, Gen: g.gen, Tool: tool}, nil)
	switch {
	case err == nil && a.Snap.Locked:
		return agent.GateTripped, nil
	case err == nil:
		return agent.GateAllowed, nil
	case errors.Is(err, lifecycle.ErrDiagnosisOnly):
		return agent.GateDiagnosisOnly, nil
	case errors.Is(err, lifecycle.ErrPaused):
		return agent.GatePaused, nil
	case errors.Is(err, lifecycle.ErrStale):
		return agent.GateSuperseded, nil
	}
	return "", err
}

func (g *runGate) Live(context.Context) agent.Verdict {
	switch {
	case g.s.paused.Load():
		return agent.GatePaused
	case g.s.runs.currentGen(g.id) != g.gen:
		return agent.GateSuperseded
	}
	return agent.GateAllowed
}

func findingOf(exit agent.RunExit) *lifecycle.Finding {
	if exit.Finding == nil || (exit.State != agent.RunConcluded && !agent.RunMeta(exit.State).WritesFinding) {
		return nil
	}
	b, err := json.Marshal(exit.Finding)
	if err != nil {
		return nil
	}
	return &lifecycle.Finding{EscalateAction: exit.Finding.EscalateAction, JSON: b}
}

func (s *Service) attemptFinished(ctx context.Context, g *runGate, exit agent.RunExit) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptWriteTimeout)
	defer cancel()
	ev := lifecycle.Event{
		Kind:    lifecycle.AttemptFinished,
		Gen:     g.gen,
		Exit:    string(exit.State),
		Finding: findingOf(exit),
	}
	extra := func(lifecycle.Snapshot, lifecycle.Decision) []*db.Event {
		if exit.Journal == nil {
			return nil
		}
		return []*db.Event{exit.Journal}
	}
	if _, err := s.apply(ctx, g.id, ev, extra); err != nil {
		s.log.ErrorContext(ctx, "record attempt finished", "incident", g.id, "exit", string(exit.State), "error", err)
	}
}
