package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	// escalationPlanTTL bounds how long an owner's approval stays valid. The
	// plan names specific file IDs, so approving one from days ago would
	// delete whatever those IDs point at now. Long enough for a human to
	// think it over, short enough that the library hasn't moved on.
	escalationPlanTTL = time.Hour
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
	plan, err := s.agent.PlanEscalation(ctx, result)
	if err != nil {
		return nil, err
	}
	// Persist exactly what the owner is about to see. ApproveEscalation
	// executes this, not a re-resolution — see its comment.
	planJSON, marshalErr := json.Marshal(plan)
	if marshalErr != nil {
		return nil, fmt.Errorf("encode escalation plan: %w", marshalErr)
	}
	if setErr := s.db.SetEscalationPlan(ctx, id, planJSON); setErr != nil {
		return nil, fmt.Errorf("store escalation plan: %w", setErr)
	}
	return plan, nil
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

	// Execute the plan the owner was actually shown. Re-resolving it at this
	// point is a TOCTOU on a destructive operation: the
	// preview lists specific files, and anything that changed in between
	// silently widens the delete, with no second confirmation.
	planJSON, previewedAt, planErr := s.db.GetEscalationPlan(ctx, id)
	if planErr != nil {
		if errors.Is(planErr, db.ErrNoEscalationPlan) {
			return errors.New("preview this escalation before approving it, so the plan " +
				"that runs is the one you saw")
		}
		return planErr
	}
	if time.Since(previewedAt) > escalationPlanTTL {
		_ = s.db.ClearEscalationPlan(ctx, id)
		return fmt.Errorf("this plan was previewed %s ago and may no longer describe the "+
			"same files; preview it again", time.Since(previewedAt).Round(time.Minute))
	}

	// Escalation execution deletes files and triggers a re-search — as
	// disruptive as anything the autonomous loop does — so it shares the same
	// global diagnostic lock a concurrent incident's Agent.Run holds (see
	// runManager.globalSlot) rather than racing it.
	if lockErr := s.runs.acquireGlobal(ctx); lockErr != nil {
		return lockErr
	}
	// Deferred, not called inline: chi's Recoverer catches a panic in
	// the escalation and returns 500, so a non-deferred release leaked the
	// single global slot permanently and every later diagnosis blocked on
	// acquireGlobal forever.
	defer s.runs.releaseGlobal()

	// ExecuteReplace is blocklist -> delete files -> trigger search, with no
	// compensation if it stops partway. Running it on the caller's HTTP
	// request context meant a closed tab or a proxy idle timeout could cancel
	// it after the files were deleted and before the re-search fired, losing
	// the media with nothing queued to replace it. WithoutCancel keeps the
	// request's values (and its deadline-free lifetime) while detaching the
	// cancellation, matching what launchVerification already does below.
	execCtx := context.WithoutCancel(ctx)
	execResult, runErr := s.agent.ExecuteApprovedPlan(execCtx, result, planJSON)
	s.logEscalation(execCtx, inc.ID, result, execResult, runErr)
	// Consumed either way: a failed run leaves the library in a state the
	// stored plan no longer describes, so it must be re-previewed too.
	if clearErr := s.db.ClearEscalationPlan(execCtx, id); clearErr != nil {
		s.log.ErrorContext(execCtx, "clear escalation plan", "incident", id, "error", clearErr)
	}
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

// logEscalation records an owner-approved escalation to the event log,
// mirroring Dispatcher.logAction but attributed to the owner rather than the
// agent. Not tagged disruptive — remove_and_search (currently the only
// automated escalation) is item-scoped, not service-wide, so it's outside
// what Agent.disruptionNote warns concurrent runs about (see
// isDisruptiveAction in internal/agent/tools.go).
func (s *Service) logEscalation(
	ctx context.Context, incidentID string, result *agent.DiagnosticResult, execResult any, runErr error,
) {
	if s.journal == nil {
		return
	}
	status := db.ActionApplied
	errMsg := ""
	if runErr != nil {
		status = db.ActionFailed
		errMsg = runErr.Error()
	}
	resultJSON, _ := json.Marshal(execResult)
	if logErr := s.journal.LogAction(ctx, &db.ActionLog{
		IncidentID:  incidentID,
		Action:      result.EscalateAction,
		Params:      result.EscalateParams,
		TriggeredBy: "owner",
		Status:      status,
		Result:      string(resultJSON),
		Error:       errMsg,
	}, false); logErr != nil {
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
