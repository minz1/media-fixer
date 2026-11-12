package incident

import (
	"context"
	"log/slog"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
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

// EscalateToOwnerForTest exposes Service.escalate for reporternotes_test.go.
func (s *Service) EscalateToOwnerForTest(ctx context.Context, inc *db.Incident, msg string) {
	if snap, err := s.db.GetIncidentSnapshot(ctx, inc.ID); err == nil {
		_, _ = s.apply(ctx, inc.ID, lifecycle.Event{Kind: lifecycle.RunClaimed, Gen: snap.Generation}, nil)
		inc.Generation = snap.Generation
	}
	s.escalate(ctx, inc, inc.Generation, lifecycle.ReasonNeedsOwner, ownerCard(inc, card.Warn, msg, ""))
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

func (s *Service) BackgroundSweepLoopForTest(ctx context.Context) {
	s.reconcileLoop(ctx)
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
	return s.routeExit(ctx, inc, inc.Generation, exit, 0, 1).done
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
	ctx, _ := s.runs.supersede(id)
	return ctx
}

func (s *Service) LockAlertArmedForTest() bool {
	return s.lockAlertAt.Load() == 0
}

func (s *Service) LockAlertAtForTest() int64 {
	return s.lockAlertAt.Load()
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
	return r.m.armCheck(id, gen, at, func(done func()) {
		defer done()
		fire()
	})
}

func (r *RunManagerForTest) ArmCheckEarlier(id string, gen int64, at time.Time, fire func()) bool {
	return r.m.armCheckEarlier(id, gen, at, func(done func()) {
		defer done()
		fire()
	})
}

func (r *RunManagerForTest) Liveness() lifecycle.Liveness {
	l := r.m.liveness()
	return lifecycle.Liveness{Runs: l.runs, Reserved: l.reserved, Plans: l.plans, Armed: l.armed}
}

func (r *RunManagerForTest) SetWake(wake func()) { r.m.wake = wake }

func (r *RunManagerForTest) End(id string) func() {
	_, tok := r.m.supersede(id)
	return func() { r.m.end(id, tok) }
}

func (r *RunManagerForTest) PlanStarted(id string) func() { return r.m.planStarted(id) }

func (s *Service) ApplyForTest(ctx context.Context, id string, ev lifecycle.Event) (db.Applied, error) {
	return s.apply(ctx, id, ev, nil)
}

func (s *Service) ArmReporterNoteForTest(ctx context.Context, id string, now time.Time) {
	s.armReporterNote(ctx, id, now)
}

func (s *Service) RearmReporterNotesForTest(ctx context.Context) { s.rearmReporterNotes(ctx) }

func (s *Service) HasProgressTimerForTest(id string) bool {
	s.progressTimersMu.Lock()
	defer s.progressTimersMu.Unlock()
	return s.progressTimers[id] != nil
}

func (s *Service) FireProgressTimerForTest(ctx context.Context, id string, now time.Time) {
	s.progressTimersMu.Lock()
	t := s.progressTimers[id]
	delete(s.progressTimers, id)
	s.progressTimersMu.Unlock()
	if t != nil {
		t.Stop()
		s.armReporterNote(ctx, id, now)
	}
}

func (s *Service) ScheduleCheckForTest(inc *db.Incident, gen int64, at time.Time) {
	s.scheduleCheck(
		inc.ID,
		lifecycle.Effect{Kind: lifecycle.ScheduleCheck, Gen: gen, At: at, Check: lifecycle.CheckVerify},
	)
}

func (s *Service) ArmedForTest(id string) int64 { return s.runs.liveness().armed[id] }

func (s *Service) ArmedAtForTest(id string) (time.Time, bool) { return s.runs.armedAt(id) }

func (s *Service) ClaimCheckForTest(id string, gen int64) func() { return s.runs.claimCheck(id, gen) }

func (s *Service) LogForTest() *slog.Logger { return s.log }

func (s *Service) CheckNowForTest(ctx context.Context, id string) {
	snap, err := s.db.GetIncidentSnapshot(ctx, id)
	if err != nil {
		return
	}
	if snap.Status == lifecycle.Verifying {
		s.verifyCheck(ctx, id, snap.Generation, snap.CheckSeq())
	}
	if snap.Status == lifecycle.AwaitingDownload {
		s.downloadCheck(ctx, id, snap.Generation, snap.CheckSeq())
	}
}

func (s *Service) DownloadCheckForTest(ctx context.Context, id string, gen int64) {
	snap, _ := s.db.GetIncidentSnapshot(ctx, id)
	s.downloadCheck(ctx, id, gen, snap.CheckSeq())
}

func (s *Service) ReservedOrRunningForTest(id string) bool {
	l := s.runs.liveness()
	_, running := l.runs[id]
	return l.reserved[id] || running
}

func (s *Service) PlanLiveForTest(id string) bool { return s.runs.liveness().plans[id] }

func (s *Service) ReconcileForTest(ctx context.Context) lifecycle.Counts { return s.reconcile(ctx) }

func (s *Service) ReserveForTest(id string) func() { return s.runs.reserve(id) }

func (s *Service) PlanStartedForTest(id string) func() { return s.runs.planStarted(id) }

func StatusLineForTest(st db.IncidentStatus) string { return statusLine(st) }

func (s *Service) RunAgentAtForTest(ctx context.Context, inc *db.Incident, gen int64) {
	s.runAgent(ctx, inc, nil, s.timers(ctx), s.gateFor(inc.ID, gen))
}

func (s *Service) GateForTest(id string, gen int64) agent.Gate { return s.gateFor(id, gen) }

func (s *Service) SetOpStoreForTest(st opexec.Store) { s.exec.Store = st }

func (s *Service) LostCauseForTest(id string) (operation.Cause, bool) { return s.runs.lostCauseOf(id) }

func (s *Service) HoldGlobalForTest(ctx context.Context) (func(), error) {
	if err := s.runs.acquireGlobal(ctx); err != nil {
		return nil, err
	}
	return s.runs.releaseGlobal, nil
}

func ResumeArgsForTest(id string, v Version, opID, opVersion int64) []string {
	return resumeArgs(id, v, opID, opVersion)
}

func (s *Service) MarkLostForTest(id string, cause operation.Cause) { s.runs.markLost(id, cause) }

func (s *Service) ClearLostForTest(id string) { s.runs.clearLost(id) }

func (s *Service) SetBeforePlanLostForTest(f func(id string)) { s.beforePlanLost = f }

func (s *Service) ApplyBareApproveForTest(ctx context.Context, id, planFP string) error {
	v := s.CurrentVersion(ctx, id)
	ev := lifecycle.Event{Kind: lifecycle.OwnerApprove, Version: v.N, HasVersion: v.Set, PlanFP: planFP}
	_, err := s.apply(ctx, id, ev, nil)
	return err
}

func EscalationVerbsForTest() []string { return escalationVerbs() }

func CardButtonVerbsForTest() []string {
	var out []string
	for _, sp := range cardSpecs(cardPlan{}, nil) {
		out = append(out, sp.verb)
	}
	return out
}

func (s *Service) SetApplyForTest(f func(ctx context.Context, id string, ev lifecycle.Event) (db.Applied, error)) {
	s.applyFn = func(ctx context.Context, id string, ev lifecycle.Event, _ eventsFor) (db.Applied, error) {
		return f(ctx, id, ev)
	}
}

func (s *Service) PausedForTest() bool { return s.paused.Load() }

func (s *Service) FlushOutboxForTest(ctx context.Context, now time.Time) { s.outbox.Flush(ctx, now) }

func SurfaceCardForTest(inc *db.Incident, result *agent.DiagnosticResult, note string) card.Card {
	return surfaceCard(inc, result, note)
}

func (s *Service) NotVerifiedForTest(inc *db.Incident) lifecycle.Notice {
	return s.notVerified(inc)
}

func (s *Service) FixedNoteForTest(inc *db.Incident) string {
	c, _, _ := s.reporterNote(inc, lifecycle.Effect{Note: lifecycle.NoteFixed}, Timers{})
	return c.Body
}
