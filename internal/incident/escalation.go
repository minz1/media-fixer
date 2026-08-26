package incident

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

// escalationVerifySeconds/Minutes size the post-approval re-check loop:
// a file delete + re-search + re-download takes longer to land than a
// library scan or cache refresh, so both are larger than their agent-driven
// counterparts (see the systemPrompt guidance in internal/agent/agent.go).
const (
	escalationVerifySeconds    = 600
	escalationVerifyETAMinutes = 15
)

// PreviewEscalation resolves an incident's recommended escalation action into
// a concrete, read-only plan (e.g. exactly which files would be deleted) for
// the dashboard's Preview button. It makes no changes.
func (s *Service) PreviewEscalation(ctx context.Context, id string) (any, error) {
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return nil, err
	}
	result, err := escalationResult(inc)
	if err != nil {
		return nil, err
	}
	return s.agent.PlanEscalation(ctx, result)
}

// ApproveEscalation executes an incident's recommended escalation after
// owner approval, logs the outcome, and drops the incident into the same
// verification loop non-destructive fixes use rather than marking it fixed
// immediately — a re-search takes time to actually resolve the incident.
func (s *Service) ApproveEscalation(ctx context.Context, id string) error {
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	result, err := escalationResult(inc)
	if err != nil {
		return err
	}

	execResult, runErr := s.agent.RunEscalation(ctx, result)
	s.logEscalation(ctx, inc.ID, result, execResult, runErr)
	if runErr != nil {
		return runErr
	}

	s.launchVerification(inc, &agent.DiagnosticResult{
		PrimaryAction:      result.EscalateAction,
		VerifyAfterSeconds: escalationVerifySeconds,
		UserETAMinutes:     escalationVerifyETAMinutes,
	})
	return nil
}

// launchVerification starts a background verification loop for inc under the
// run manager, the same cancel-on-supersede context lifecycle launch() gives
// full agent runs — so it outlives the HTTP request that triggered it, and a
// later reopen/reinvestigate cancels it cleanly instead of racing it.
func (s *Service) launchVerification(inc *db.Incident, result *agent.DiagnosticResult) {
	ctx, tok := s.runs.begin(inc.ID)
	go func() {
		defer s.runs.end(inc.ID, tok)
		s.runVerification(ctx, inc, result)
	}()
}

// logEscalation records an owner-approved escalation to the action log,
// mirroring Dispatcher.logAction but attributed to the owner rather than the
// agent.
func (s *Service) logEscalation(
	ctx context.Context, incidentID string, result *agent.DiagnosticResult, execResult any, runErr error,
) {
	status := db.ActionApplied
	errMsg := ""
	if runErr != nil {
		status = db.ActionFailed
		errMsg = runErr.Error()
	}
	resultJSON, _ := json.Marshal(execResult)
	if logErr := s.db.LogAction(ctx, &db.ActionLog{
		IncidentID:  incidentID,
		Action:      result.EscalateAction,
		Params:      result.EscalateParams,
		TriggeredBy: "owner",
		Status:      status,
		Result:      string(resultJSON),
		Error:       errMsg,
	}); logErr != nil {
		s.log.ErrorContext(ctx, "log escalation action", "incident", incidentID, "error", logErr)
	}
}

// escalationResult reconstructs the agent.DiagnosticResult from an incident's
// stored finding (persisted generically as `any` by db.SetIncidentFinding) so
// PreviewEscalation/ApproveEscalation can reuse Agent's escalation methods.
func escalationResult(inc *db.Incident) (*agent.DiagnosticResult, error) {
	if inc.Finding == nil {
		return nil, fmt.Errorf("incident %s has no diagnostic finding", inc.ID)
	}
	b, err := json.Marshal(inc.Finding)
	if err != nil {
		return nil, fmt.Errorf("re-marshal finding: %w", err)
	}
	var result agent.DiagnosticResult
	if unmarshalErr := json.Unmarshal(b, &result); unmarshalErr != nil {
		return nil, fmt.Errorf("decode finding: %w", unmarshalErr)
	}
	if result.EscalateAction == "" || result.EscalateAction == agent.EscalateNone {
		return nil, fmt.Errorf("incident %s has no escalation action recommended", inc.ID)
	}
	return &result, nil
}
