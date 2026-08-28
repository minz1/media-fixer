package incident

import (
	"context"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

// ActionAlreadyTriedForTest exposes Service.actionAlreadyTried for service_test.go.
func (s *Service) ActionAlreadyTriedForTest(ctx context.Context, incidentID, action string) bool {
	return s.actionAlreadyTried(ctx, incidentID, action)
}

// ReviewRiskReasonForTest exposes Service.reviewRiskReason for service_test.go.
func (s *Service) ReviewRiskReasonForTest(
	ctx context.Context, incidentID string, result *agent.DiagnosticResult,
) string {
	return s.reviewRiskReason(ctx, incidentID, result)
}

// ControlProposalForTest exposes controlProposal for service_test.go.
func ControlProposalForTest(result *agent.DiagnosticResult, riskReason, actionHistory string) string {
	return controlProposal(result, riskReason, actionHistory)
}

// DistinctAppliedActionCountForTest exposes Service.distinctAppliedActionCount.
func (s *Service) DistinctAppliedActionCountForTest(ctx context.Context, incidentID string) int {
	return s.distinctAppliedActionCount(ctx, incidentID)
}

// NotifyReportersForTest exposes Service.notifyReporters for service_test.go.
func (s *Service) NotifyReportersForTest(ctx context.Context, inc *db.Incident, msg string) {
	s.notifyReporters(ctx, inc, msg)
}
