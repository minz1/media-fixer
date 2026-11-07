package incident

import (
	"context"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
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

func (s *Service) ClaimSeerrAlertForTest(now time.Time, gap time.Duration) (time.Time, bool) {
	return s.claimSeerrAlert(now, gap)
}

func (s *Service) BackgroundSweepLoopForTest(ctx context.Context) {
	s.backgroundSweepLoop(ctx)
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

func (s *Service) EndLockEpisodeForTest(ctx context.Context) {
	s.endLockEpisode(ctx)
}

func (s *Service) SetUnitForTest(d time.Duration) {
	s.unit.Store(int64(d))
}

func ErrNotAwaitingDecisionForTest() error { return errNotAwaitingDecision }

func ErrPlanChangedForTest() error { return errPlanChanged }

func ErrPlanExpiredForTest() error { return errPlanExpired }

func (s *Service) RunningForTest(id string) bool { return s.runs.running(id) }

func (s *Service) RouteExitForTest(ctx context.Context, inc *db.Incident, exit agent.RunExit) bool {
	return s.routeExit(ctx, inc, exit, 0, 1).done
}

func (s *Service) RunAgentForTest(ctx context.Context, inc *db.Incident) {
	s.runAgent(ctx, inc, nil, s.timers(ctx), s.gateFor(inc.ID, inc.Generation))
}

type WorkersForTest struct{ w *workers }

func NewWorkersForTest(parent context.Context) *WorkersForTest {
	return &WorkersForTest{w: newWorkers(parent)}
}

func (w *WorkersForTest) GoNamed(name string, fn func(context.Context)) bool {
	return w.w.goNamed(name, fn)
}

func (w *WorkersForTest) Enter(name string) (func(), bool) { return w.w.enter(name) }

func (w *WorkersForTest) Stop() { w.w.stop() }

func (w *WorkersForTest) Wait(ctx context.Context) []string { return w.w.wait(ctx) }

func (s *Service) SupersedeForTest(id string) context.Context {
	ctx, _, _ := s.runs.supersede(id)
	return ctx
}

func (s *Service) LockAlertArmedForTest() bool {
	return s.lockAlertAt.Load() == 0
}

type RunManagerForTest struct{ m *runManager }

func NewRunManagerForTest(base context.Context) *RunManagerForTest {
	return &RunManagerForTest{m: newRunManager(base)}
}

func (r *RunManagerForTest) SupersedeGen(id string, gen int64) (context.Context, bool) {
	ctx, _, ok := r.m.supersedeGen(id, gen)
	return ctx, ok
}

func (r *RunManagerForTest) Reserve(id string) func() { return r.m.reserve(id) }

func (r *RunManagerForTest) CancelBelow(id string, gen int64) { r.m.cancelBelow(id, gen) }

func (r *RunManagerForTest) ArmCheck(id string, gen int64, at time.Time, fire func()) bool {
	return r.m.armCheck(id, gen, at, fire)
}

func (r *RunManagerForTest) Liveness() lifecycle.Liveness {
	l := r.m.liveness()
	return lifecycle.Liveness{Runs: l.runs, Reserved: l.reserved, Plans: l.plans, Armed: l.armed}
}

func (s *Service) ApplyForTest(ctx context.Context, id string, ev lifecycle.Event) (db.Applied, error) {
	return s.apply(ctx, id, ev, nil)
}

func (s *Service) ScheduleCheckForTest(inc *db.Incident, gen int64, at time.Time) {
	s.scheduleCheck(
		inc,
		lifecycle.Effect{Kind: lifecycle.ScheduleCheck, Gen: gen, At: at, Check: lifecycle.CheckVerify},
	)
}
