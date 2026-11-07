package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

const nullResult = "null"

var (
	errNotAwaitingDecision = errors.New("this incident is not waiting for an escalation decision")
	errNoPlan              = errors.New("preview this escalation before approving it, so the plan " +
		"that runs is the one you saw")
	errPlanExpired = errors.New("may no longer describe the same files; preview it again")
	errPlanChanged = errors.New("the plan changed since you previewed it; preview it again")
)

// PreviewEscalation resolves an incident's recommended escalation action into
// a concrete, read-only plan (e.g. exactly which files would be deleted) for
// the dashboard's Preview button. It makes no changes.
func (s *Service) PreviewEscalation(ctx context.Context, id string, v Version) (any, string, error) {
	leave, err := s.ownerSection("preview", id)
	if err != nil {
		return nil, "", err
	}
	defer leave()
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if inc.Status != db.StatusManualTestNeeded {
		return nil, "", errNotAwaitingDecision
	}
	if r := lifecycle.Reason(inc.Reason); r == lifecycle.ReasonPlanInterrupted || r == lifecycle.ReasonPlanPartial {
		return nil, "", lifecycle.ErrPlanHalfRun
	}
	result, err := escalationResult(inc)
	if err != nil {
		return nil, "", err
	}
	plan, err := s.agent.PlanEscalation(ctx, result, inc.What)
	if err != nil {
		return nil, "", err
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, "", fmt.Errorf("encode escalation plan: %w", err)
	}
	fp := card.Fingerprint(planJSON)
	ev := lifecycle.Event{Kind: lifecycle.OwnerPreview, Version: v.N, HasVersion: v.Set, Plan: planJSON, PlanFP: fp}
	if _, err = s.apply(ctx, id, ev, nil); err != nil {
		return nil, "", err
	}
	return plan, fp, nil
}

// ApproveEscalation executes an incident's recommended escalation after
// owner approval, logs the outcome, and drops the incident into the same
// verification loop non-destructive fixes use rather than marking it fixed
// immediately — a re-search takes time to actually resolve the incident.
func (s *Service) ApproveEscalation(ctx context.Context, id, planFp string, v Version) error {
	if _, err := s.approvable(ctx, id, planFp); err != nil {
		return err
	}
	if err := s.runs.acquireGlobal(ctx); err != nil {
		return err
	}
	defer s.runs.releaseGlobal()
	leave, err := s.ownerSection("approve", id)
	if err != nil {
		return err
	}
	defer leave()
	planDone := s.runs.planStarted(id)
	defer planDone()
	ev := lifecycle.Event{Kind: lifecycle.OwnerApprove, Version: v.N, HasVersion: v.Set, PlanFP: planFp}
	a, err := s.apply(ctx, id, ev, nil)
	if err != nil {
		return err
	}
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	result, err := escalationResult(inc)
	if err != nil {
		return err
	}
	execCtx := context.WithoutCancel(ctx)
	execResult, runErr := s.agent.ExecuteApprovedPlan(execCtx, result, a.Snap.Plan.JSON)
	s.logEscalation(execCtx, inc.ID, result, execResult, runErr)
	done := lifecycle.Event{Kind: lifecycle.PlanExecuted, Gen: a.Snap.Generation}
	if runErr != nil {
		done.Err, done.Partial = runErr.Error(), partialOf(result.EscalateAction, execResult)
	}
	if _, err = s.apply(execCtx, id, done, nil); err != nil {
		s.logCheckErr(execCtx, id, err)
	}
	return runErr
}

// approvable returns the plan the owner was actually shown. Re-resolving it at
// this point is a TOCTOU on a destructive operation: the preview lists specific
// files, and anything that changed in between silently widens the delete, with
// no second confirmation.
func (s *Service) approvable(
	ctx context.Context, id, planFp string,
) (*agent.DiagnosticResult, error) {
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return nil, err
	}
	if inc.Status != db.StatusManualTestNeeded {
		return nil, errNotAwaitingDecision
	}
	result, err := escalationResult(inc)
	if err != nil {
		return nil, err
	}
	planJSON, previewedAt, err := s.db.GetEscalationPlan(ctx, id)
	if errors.Is(err, db.ErrNoEscalationPlan) {
		return nil, errNoPlan
	}
	if err != nil {
		return nil, err
	}
	if age := time.Since(previewedAt); age > s.minutes(planTTLMinutes) {
		return nil, fmt.Errorf(
			"this plan was previewed %s ago and %w",
			age.Round(time.Minute),
			errPlanExpired,
		)
	}
	if card.Fingerprint(planJSON) != planFp {
		return nil, errPlanChanged
	}
	return result, nil
}

func partialOf(action string, execResult any) lifecycle.Partial {
	b, err := json.Marshal(execResult)
	unreadable := lifecycle.Partial{Applied: true, Line: "Partial: the result could not be read"}
	if err != nil || string(b) == nullResult {
		return unreadable
	}
	switch action {
	case agent.EscalateRemoveAndSearch:
		var r client.ReplaceResult
		if json.Unmarshal(b, &r) != nil {
			return unreadable
		}
		search := "search not sent"
		if r.SearchTriggered != "" {
			search = "search sent"
		}
		return lifecycle.Partial{
			Applied: len(r.DeletedFiles) > 0 || len(r.BlocklistedGrabs) > 0 || r.SearchTriggered != "",
			Line: fmt.Sprintf("Partial: deleted %d files, blocklisted %d, %s",
				len(r.DeletedFiles), len(r.BlocklistedGrabs), search),
		}
	case agent.EscalateDeleteTorrentReadd:
		var r client.ReaddResult
		if json.Unmarshal(b, &r) != nil {
			return unreadable
		}
		return lifecycle.Partial{Applied: r.Deleted, Line: "Partial: torrent deleted, re-add failed"}
	}
	return unreadable
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
	if string(resultJSON) == nullResult {
		resultJSON = nil
	}
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
