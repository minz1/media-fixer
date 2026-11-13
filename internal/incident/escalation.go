package incident

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

const nullResult = "null"

const opSettleBound = 5 * time.Second

var (
	errNotAwaitingDecision = lifecycle.ErrNotAwaitingDecision
	errNoPlan              = lifecycle.ErrNoPlan
	errPlanExpired         = lifecycle.ErrPlanExpired
	errPlanChanged         = lifecycle.ErrPlanChanged
	errOpUnsettled         = errors.New("the approved plan could not be recorded: database error")
	errOpStopped           = errors.New("the approved plan stopped before it finished")
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
	result, planJSON, err := s.approvable(ctx, id, planFp)
	if err != nil {
		return err
	}
	if err = s.runs.acquireGlobal(ctx); err != nil {
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
	spec, buildErr := s.agent.OperationFor(
		operation.Subject{Kind: operation.SubjectIncident, ID: id}, result, planJSON)
	ev := lifecycle.Event{Kind: lifecycle.OwnerApprove, Version: v.N, HasVersion: v.Set, PlanFP: planFp}
	if buildErr != nil {
		ev.BuildErr = buildErr.Error()
	} else {
		ev.Spec = &spec
	}
	a, err := s.apply(ctx, id, ev, nil)
	if err != nil {
		return err
	}
	if buildErr != nil {
		return buildErr
	}
	if a.Snap.Op == nil {
		return fmt.Errorf("incident %s: approval created no operation", id)
	}
	return s.runApprovedOp(ctx, id, result, a)
}

func (s *Service) runApprovedOp(ctx context.Context, id string, result *agent.DiagnosticResult, a db.Applied) error {
	execCtx := context.WithoutCancel(ctx)
	op := s.exec.Run(execCtx, *a.Snap.Op)
	if op.Status == operation.Running {
		op = s.interruptOp(execCtx, op)
	}
	s.logEscalation(execCtx, id, result, op)
	settled := s.settlePlan(execCtx, id, a)
	if !settled || op.Status == operation.Running {
		s.runs.markLost(id, operation.CauseStore)
	}
	return opError(op)
}

func (s *Service) interruptOp(ctx context.Context, op operation.Op) operation.Op {
	wctx, cancel := context.WithTimeout(ctx, opSettleBound)
	defer cancel()
	stopped, err := s.exec.Store.ApplyOperation(wctx, op.ID, func(cur operation.Op) (operation.Op, error) {
		return operation.Next(cur, operation.Event{
			Kind: operation.Interrupted, At: time.Now(), Cause: operation.CauseStore,
		})
	})
	if err != nil {
		s.log.ErrorContext(ctx, "interrupt operation after a store failure", "op", op.ID, "error", err)
		return op
	}
	return stopped
}

func (s *Service) settlePlan(ctx context.Context, id string, a db.Applied) bool {
	wctx, cancel := context.WithTimeout(ctx, opSettleBound)
	defer cancel()
	done := lifecycle.Event{Kind: lifecycle.PlanExecuted, Gen: a.Snap.Generation, OpID: a.Snap.Op.ID}
	_, err := s.apply(wctx, id, done, nil)
	if err == nil || errors.Is(err, lifecycle.ErrStale) {
		return true
	}
	s.logCheckErr(ctx, id, err)
	return false
}

func opError(op operation.Op) error {
	switch op.Status {
	case operation.Done:
		return nil
	case operation.Running:
		return errOpUnsettled
	case operation.None, operation.FailedAtStep:
	}
	if op.Index >= 0 && op.Index < len(op.Results) {
		r := op.Results[op.Index]
		if text := cmp.Or(r.Err, r.Note); text != "" {
			return errors.New(text)
		}
	}
	return errOpStopped
}

// approvable returns the plan the owner was actually shown. Re-resolving it at
// this point is a TOCTOU on a destructive operation: the preview lists specific
// files, and anything that changed in between silently widens the delete, with
// no second confirmation.
func (s *Service) approvable(
	ctx context.Context, id, planFp string,
) (*agent.DiagnosticResult, []byte, error) {
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if inc.Status != db.StatusManualTestNeeded {
		return nil, nil, errNotAwaitingDecision
	}
	result, err := escalationResult(inc)
	if err != nil {
		return nil, nil, err
	}
	planJSON, previewedAt, err := s.db.GetEscalationPlan(ctx, id)
	if errors.Is(err, db.ErrNoEscalationPlan) {
		return nil, nil, errNoPlan
	}
	if err != nil {
		return nil, nil, err
	}
	if age := time.Since(previewedAt); age > s.minutes(planTTLMinutes) {
		return nil, nil, fmt.Errorf(
			"this plan was previewed %s ago and %w",
			age.Round(time.Minute),
			errPlanExpired,
		)
	}
	if card.Fingerprint(planJSON) != planFp {
		return nil, nil, errPlanChanged
	}
	return result, planJSON, nil
}

func partialOf(action string, execResult any) string {
	b, err := json.Marshal(execResult)
	const unreadable = "Partial: the result could not be read"
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
		return fmt.Sprintf("Partial: deleted %d files, blocklisted %d, %s",
			len(r.DeletedFiles), len(r.BlocklistedGrabs), search)
	case agent.EscalateDeleteTorrentReadd:
		var r client.ReaddResult
		if json.Unmarshal(b, &r) != nil {
			return unreadable
		}
		return "Partial: torrent deleted, re-add failed"
	}
	return unreadable
}

// logEscalation records an owner-approved escalation to the event log,
// mirroring Agent.recordCall but attributed to the owner rather than the
// agent.
func (s *Service) logEscalation(
	ctx context.Context, incidentID string, result *agent.DiagnosticResult, op operation.Op,
) {
	if s.journal == nil {
		return
	}
	status := db.ActionApplied
	errMsg := ""
	if runErr := opError(op); runErr != nil {
		status = db.ActionFailed
		errMsg = runErr.Error()
	}
	if logErr := s.journal.LogAction(ctx, &db.ActionLog{
		IncidentID:  incidentID,
		Action:      result.EscalateAction,
		Params:      result.EscalateParams,
		TriggeredBy: "owner",
		Status:      status,
		Result:      opResultJSON(result.EscalateAction, op),
		Error:       errMsg,
	}, agent.Disruptive(result.EscalateAction)); logErr != nil {
		s.log.ErrorContext(ctx, "log escalation action", "incident", incidentID, "error", logErr)
	}
}

type replaceLog struct {
	client.ReplaceResult

	Reason       string `json:"reason"`
	ReadableFile string `json:"readable_file"`
}

func opResultJSON(action string, op operation.Op) string {
	var out any
	switch action {
	case agent.EscalateRemoveAndSearch:
		entry := replaceLog{ReplaceResult: opexec.ReplaceResultFrom(op)}
		var plan client.ReplacePlan
		if json.Unmarshal(op.Source, &plan) == nil {
			entry.Reason = plan.Reason
			if plan.Readability != nil {
				entry.ReadableFile = plan.Readability.ReadableFile
			}
		}
		out = entry
	case agent.EscalateDeleteTorrentReadd:
		out = opexec.ReaddResultFrom(op)
	default:
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// escalationResult reconstructs the agent.DiagnosticResult from an incident's
// stored finding (persisted generically as `any` by db.ApplyIncident) so
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

type opRefusalError struct{ note string }

func (e *opRefusalError) Error() string { return e.note }

func RefusalNote(err error) string {
	if r, ok := errors.AsType[*opRefusalError](err); ok {
		return r.note
	}
	return ""
}

func refuseOp(err error) error {
	rej, ok := errors.AsType[*operation.RejectError](err)
	switch {
	case !ok:
		return err
	case rej.Code == operation.CodeConflict && rej.Note != "":
		return &opRefusalError{note: rej.Note}
	case rej.Code == operation.CodeChanged || rej.Code == operation.CodeProbesStale:
		return lifecycle.ErrChanged
	case rej.Code == operation.CodeNotResumable:
		return lifecycle.ErrNotResumable
	}
	return err
}

func (s *Service) resumeTarget(ctx context.Context, id string, opID, opVersion int64) (lifecycle.Snapshot, error) {
	snap, err := s.db.GetIncidentSnapshotWithOp(ctx, id)
	if err != nil {
		return snap, err
	}
	planned := snap.Reason == lifecycle.ReasonPlanPartial || snap.Reason == lifecycle.ReasonPlanInterrupted
	switch {
	case snap.Status != lifecycle.ManualTestNeeded || !planned || snap.Op == nil || !operation.Resumable(*snap.Op):
		return snap, lifecycle.ErrNotResumable
	case snap.Op.ID != opID || snap.Op.Version != opVersion:
		return snap, lifecycle.ErrChanged
	}
	return snap, nil
}

func (s *Service) ResumeOperation(ctx context.Context, id string, v Version, opID, opVersion int64) error {
	if _, err := s.resumeTarget(ctx, id, opID, opVersion); err != nil {
		return err
	}
	if err := s.runs.acquireGlobal(ctx); err != nil {
		return err
	}
	defer s.runs.releaseGlobal()
	leave, err := s.ownerSection("resume", id)
	if err != nil {
		return err
	}
	defer leave()
	planDone := s.runs.planStarted(id)
	defer planDone()
	snap, err := s.resumeTarget(ctx, id, opID, opVersion)
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
	probes, err := s.exec.Probe(ctx, *snap.Op)
	if err != nil {
		return fmt.Errorf("couldn't check the live state: %w; try again", err)
	}
	ev := lifecycle.Event{
		Kind: lifecycle.OwnerResumeOp, Version: v.N, HasVersion: v.Set, OpID: opID, OpVersion: opVersion,
		Probes: probes, PlanFP: card.Fingerprint(snap.Op.Source),
	}
	a, err := s.apply(ctx, id, ev, nil)
	if err != nil {
		return refuseOp(err)
	}
	if !slices.ContainsFunc(a.Effects, func(e lifecycle.Effect) bool { return e.Kind == lifecycle.ExecutePlan }) {
		return nil
	}
	if a.Snap.Op == nil {
		return fmt.Errorf("incident %s: resume produced no operation", id)
	}
	return s.runApprovedOp(ctx, id, fromStep(result, a.Snap.Op.Index+1), a)
}

func fromStep(result *agent.DiagnosticResult, step int) *agent.DiagnosticResult {
	out := *result
	out.EscalateParams = make(map[string]any, len(result.EscalateParams)+1)
	maps.Copy(out.EscalateParams, result.EscalateParams)
	out.EscalateParams["from_step"] = step
	return &out
}
