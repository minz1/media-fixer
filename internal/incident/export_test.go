package incident

import (
	"context"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
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
	s.notifyReporters(ctx, inc, reporterCard(card.Info, msg))
}

// EscalateToOwnerForTest exposes Service.escalateToOwner for reporternotes_test.go.
func (s *Service) EscalateToOwnerForTest(ctx context.Context, inc *db.Incident, msg string) {
	s.escalateToOwner(ctx, inc, ownerCard(inc, card.Warn, msg, ""))
}

// ProgressPhrasesForTest exposes progressPhrases for progress_test.go.
func ProgressPhrasesForTest(actions []*db.ActionLog, since time.Time, po *db.PendingOutcome) []string {
	return progressPhrases(actions, since, po)
}

// BackdateSeerrAlertForTest moves the last Seerr failure DM d into the past.
func (s *Service) BackdateSeerrAlertForTest(d time.Duration) {
	s.seerrAlertMu.Lock()
	defer s.seerrAlertMu.Unlock()
	s.seerrAlertAt = s.seerrAlertAt.Add(-d)
}

func (s *Service) ClaimSeerrAlertForTest(now time.Time) (time.Time, bool) {
	return s.claimSeerrAlert(now)
}

func (s *Service) ReleaseSeerrAlertForTest(claimed, prev time.Time) {
	s.releaseSeerrAlert(claimed, prev)
}

func (s *Service) SeerrAlertAtForTest() time.Time {
	s.seerrAlertMu.Lock()
	defer s.seerrAlertMu.Unlock()
	return s.seerrAlertAt
}

func (s *Service) SetSeedTimeoutForTest(d time.Duration) {
	s.seedTimeout = d
}
