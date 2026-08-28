package incident

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

const systematicIncidentThreshold = 5

const (
	retryDelay1 = 2 * time.Minute
	retryDelay2 = 6 * time.Minute
)

const (
	// maxVerifyLoops bounds how many times a deferred fix is re-checked before
	// the system gives up and either reports an ETA or escalates.
	maxVerifyLoops = 5
	// verifyLoopDelayCap caps each verification wait so the goroutine can't sleep
	// for an unbounded agent-supplied duration.
	verifyLoopDelayCap = 2 * time.Minute
	// defaultUserETAMinutes is the fallback "try again in N minutes" estimate.
	defaultUserETAMinutes = 10
)

const (
	// staleRunThreshold is how long a run can sit in "investigating" without a
	// heartbeat before SweepStaleRuns treats it as hung rather than mid-retry.
	// Comfortably above retryDelay1+retryDelay2 (8 minutes total), the longest
	// gap a legitimately-retrying run can go without writing one — Run's own
	// TransitionStatus keeps status at "investigating" across retries, and
	// Agent.logTurn (the heartbeat write) doesn't fire again until the next
	// attempt's first LLM round completes.
	staleRunThreshold = 15 * time.Minute
	// staleRunSweepInterval is how often SweepStaleRuns runs in the background.
	staleRunSweepInterval = 5 * time.Minute
)

// Service manages incident lifecycle and orchestrates agent runs.
type Service struct {
	db         *db.DB
	agent      AgentRunner
	control    *agent.ControlReviewer
	summarizer *agent.Summarizer
	notif      Notifier
	log        *slog.Logger
	runs       *runManager
}

// Notifier is implemented by the Discord bot to send DMs.
type Notifier interface {
	NotifyOwner(ctx context.Context, msg string) error
	NotifyUser(ctx context.Context, userID, msg string) error
}

// NewService wires up the incident service. base is the process-lifetime context
// (e.g. from signal.NotifyContext); all background agent runs derive from it, so
// shutdown cancels them.
func NewService(
	base context.Context,
	database *db.DB,
	ag AgentRunner,
	control *agent.ControlReviewer,
	summarizer *agent.Summarizer,
	notif Notifier,
	log *slog.Logger,
) *Service {
	s := &Service{
		db:         database,
		agent:      ag,
		control:    control,
		summarizer: summarizer,
		notif:      notif,
		log:        log,
		runs:       newRunManager(base),
	}
	go s.sweepStaleRunsLoop(base)
	return s
}

// launch starts a background agent run for an incident with a fixed seed (nil
// for a fresh run). Thin wrapper over launchWithSeed for callers that already
// have a fully-built seed in hand.
func (s *Service) launch(inc *db.Incident, seed []openai.ChatCompletionMessage) {
	s.launchWithSeed(inc, func(context.Context) []openai.ChatCompletionMessage { return seed })
}

// launchWithSeed starts (or restarts) the background agent pipeline for an
// incident under the run manager. Because begin() cancels any in-flight run for
// the same incident, a rerun supersedes a prior run instead of racing it.
// buildSeed runs on the new run's own (background) context rather than the
// caller's, so a seed built from an expensive call (buildReinvestigateSeed
// summarizes the prior conversation via an LLM call) never blocks — or gets
// silently dropped by — an HTTP request context.
func (s *Service) launchWithSeed(inc *db.Incident, buildSeed func(context.Context) []openai.ChatCompletionMessage) {
	ctx, tok := s.runs.begin(inc.ID)
	go func() {
		defer s.runs.end(inc.ID, tok)
		seed := buildSeed(ctx)
		s.runAgent(ctx, inc, seed)
	}()
}

// Report is the normalised form of an incoming issue report from any source.
type Report struct {
	Source            string // "discord" | "seerr"
	ReportedBy        string
	ReporterDiscordID string // empty for non-Discord sources
	What              string // "cant_play" | "login_failed" | "missing_media" | "other"
	Title             string
	JellyfinItemID    string
	Details           string
}

// Handle processes a new report: deduplicates, creates/updates the incident,
// and starts the agent in the background if this is a new incident.
func (s *Service) Handle(ctx context.Context, r *Report) (*db.Incident, error) {
	existing, err := s.db.FindOpenByTitle(ctx, r.Title)
	switch {
	case err == nil:
		s.log.InfoContext(ctx, "duplicate report collapsed", "incident", existing.ID, "reporter", r.ReportedBy)
		_ = s.db.AddReporter(ctx, existing.ID, r.ReportedBy, r.Source, r.ReporterDiscordID)
		return existing, nil
	case !errors.Is(err, db.ErrNotFound):
		return nil, fmt.Errorf("find open incident: %w", err)
	}

	openCount, err := s.db.CountOpenIncidents(ctx)
	if err != nil {
		return nil, err
	}

	inc := &db.Incident{
		Status:         db.StatusOpen,
		Source:         r.Source,
		ReportedBy:     r.ReportedBy,
		What:           r.What,
		Title:          r.Title,
		JellyfinItemID: r.JellyfinItemID,
		Details:        r.Details,
	}
	if err = s.db.CreateIncident(ctx, inc); err != nil {
		return nil, fmt.Errorf("create incident: %w", err)
	}
	_ = s.db.AddReporter(ctx, inc.ID, r.ReportedBy, r.Source, r.ReporterDiscordID)

	if openCount >= systematicIncidentThreshold {
		_ = s.db.SetAutonomousLocked(ctx, inc.ID, true)
		_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusBlocked)
		msg := fmt.Sprintf(
			"⚠️ %d open incidents — possible systemic failure. Autonomous actions locked. New incident: **%s** (#%s)",
			openCount+1,
			r.Title,
			inc.ID[:8],
		)
		if notifyErr := s.notif.NotifyOwner(ctx, msg); notifyErr != nil {
			s.log.ErrorContext(ctx, "notify owner", "error", notifyErr)
		}
		return inc, nil
	}

	s.launch(inc, nil)

	return inc, nil
}

func (s *Service) runAgent(ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage) {
	if s.agent == nil {
		return
	}

	paused, err := s.db.IsAutonomousPaused(ctx)
	if err != nil || paused {
		s.log.WarnContext(ctx, "autonomous actions paused, skipping agent", "incident", inc.ID)
		return
	}

	// A locked incident is not acted on autonomously. Manual paths (Reopen,
	// Reinvestigate) clear the lock first, so an explicit human override still runs.
	if inc.AutonomousLocked {
		s.log.WarnContext(ctx, "incident autonomous-locked, skipping agent", "incident", inc.ID)
		return
	}

	retryDelays := []time.Duration{0, retryDelay1, retryDelay2}

	for attempt, delay := range retryDelays {
		if delay > 0 {
			time.Sleep(delay)
		}

		result, conversation, runErr := s.agent.Run(ctx, inc, seed)
		if runErr != nil {
			s.handleRunError(ctx, inc, runErr)
			return
		}

		outcome := s.evaluateDiagnosis(ctx, inc, result, conversation, attempt, len(retryDelays))
		if outcome.done {
			return
		}
		seed = outcome.seed
	}
}

// diagnosisOutcome is what runAgent's retry loop does next after evaluating
// one agent.Run result: stop (the incident reached a terminal state for this
// call) or retry with an updated seed (a control reviewer suggested trying
// something else).
type diagnosisOutcome struct {
	done bool
	seed []openai.ChatCompletionMessage
}

// evaluateDiagnosis routes one Run() result through control review — not
// only when the model itself flagged requires_approval, but also when its
// own confidence is low or it just proposed repeating an action already
// tried (and failed) on this incident. Gating review purely on the model's
// self-assessment meant the reviewer only ever saw what the model chose to
// flag about itself — in a week of production traffic, that was never once,
// even on incidents re-diagnosed three times with the same "high
// confidence" action each time.
func (s *Service) evaluateDiagnosis(
	ctx context.Context,
	inc *db.Incident,
	result *agent.DiagnosticResult,
	conversation []openai.ChatCompletionMessage,
	attempt, maxAttempts int,
) diagnosisOutcome {
	riskReason := s.reviewRiskReason(ctx, inc.ID, result)
	if !result.RequiresApproval && riskReason == "" {
		s.handleAgentResolved(ctx, inc, result)
		return diagnosisOutcome{done: true}
	}

	if s.control == nil {
		if result.RequiresApproval {
			s.surfaceToOwner(ctx, inc, result, "")
		} else {
			// No reviewer configured to double-check a repeat/low-confidence
			// proposal the model didn't itself flag — proceed rather than block
			// on a review that can never happen.
			s.handleAgentResolved(ctx, inc, result)
		}
		return diagnosisOutcome{done: true}
	}

	verdict, verdictErr := s.control.Review(ctx, conversation, controlProposal(result, riskReason))
	if verdictErr != nil {
		s.log.ErrorContext(ctx, "control review error", "incident", inc.ID, "error", verdictErr)
		if result.RequiresApproval {
			s.surfaceToOwner(ctx, inc, result, " (control review failed)")
		} else {
			s.handleAgentResolved(ctx, inc, result)
		}
		return diagnosisOutcome{done: true}
	}

	switch verdict.Verdict {
	case agent.VerdictApprove:
		if result.RequiresApproval {
			s.surfaceToOwner(ctx, inc, result, "")
		} else {
			// The reviewer approved a fix the model didn't itself flag for
			// approval (this review was triggered by our own repeat/low-
			// confidence heuristic) — let it proceed autonomously as proposed.
			s.handleAgentResolved(ctx, inc, result)
		}
		return diagnosisOutcome{done: true}

	case agent.VerdictEscalateToOwner:
		s.escalateToOwner(ctx, inc, fmt.Sprintf(
			"⚠️ Control reviewer flagged a potentially unreliable diagnosis for **%s** (#%s).\nRoot cause: %s\nConcern: %s",
			inc.Title,
			inc.ID[:8],
			result.RootCause,
			verdict.Reason,
		))
		return diagnosisOutcome{done: true}

	case agent.VerdictSuggestAlternative:
		if attempt == maxAttempts-1 {
			s.escalateToOwner(ctx, inc, fmt.Sprintf(
				"⚠️ **%s** (#%s): agent still needs approval after %d retries.\nRoot cause: %s\nAction needed: %s",
				inc.Title, inc.ID[:8], attempt+1, result.RootCause, agent.EscalationSummary(result),
			))
			return diagnosisOutcome{done: true}
		}
		feedback := openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleUser,
			Content: fmt.Sprintf(
				"Control review suggests an alternative approach: %s\n\nPlease re-evaluate and try this approach before escalating.",
				verdict.AlternativeAction,
			),
		}
		conversation = append(conversation, feedback)
		s.log.InfoContext(ctx, "control reviewer suggested alternative, retrying",
			"incident", inc.ID,
			"attempt", attempt+1,
			"alternative", verdict.AlternativeAction,
		)
		return diagnosisOutcome{done: false, seed: conversation}
	}

	// Unreachable: ControlVerdict.Verdict is validated by ControlReviewer.Review
	// to one of the three cases above before it can ever be returned.
	return diagnosisOutcome{done: true}
}

// reviewRiskReason reports why an otherwise-autonomous diagnosis (one the
// model itself didn't flag requires_approval for) should still be routed
// through control review, or "" if it shouldn't need one. Checked in addition
// to result.RequiresApproval in runAgent.
func (s *Service) reviewRiskReason(ctx context.Context, incidentID string, result *agent.DiagnosticResult) string {
	if strings.EqualFold(result.Confidence, "low") {
		return "the agent's own confidence in this diagnosis is low"
	}
	if s.actionAlreadyTried(ctx, incidentID, result.PrimaryAction) {
		return fmt.Sprintf("%q was already applied on this incident and did not resolve it", result.PrimaryAction)
	}
	return ""
}

// actionAlreadyTried reports whether action has already been logged as
// successfully applied on this incident.
func (s *Service) actionAlreadyTried(ctx context.Context, incidentID, action string) bool {
	if action == "" {
		return false
	}
	actions, err := s.db.ListActions(ctx, incidentID)
	if err != nil {
		return false
	}
	for _, act := range actions {
		if act.Action == action && act.Status == db.ActionApplied {
			return true
		}
	}
	return false
}

// controlProposal builds the description of what's being reviewed and why,
// passed to ControlReviewer.Review — see controlSystemPrompt in
// internal/agent/control.go for how the two trigger cases are meant to read.
func controlProposal(result *agent.DiagnosticResult, riskReason string) string {
	if result.RequiresApproval {
		return fmt.Sprintf(
			"The agent wants to escalate the following action for owner approval:\n\n%s",
			agent.EscalationSummary(result),
		)
	}
	return fmt.Sprintf(
		"The agent proposes to autonomously apply %q (not flagged for owner approval), but this "+
			"review was triggered because %s.\n\nRoot cause: %s\nReason: %s",
		result.PrimaryAction, riskReason, result.RootCause, result.PrimaryReason,
	)
}

// handleAgentResolved processes a non-approval diagnosis: either it kicks off the
// verification loop (when the fix needs time) or marks the incident fixed now.
func (s *Service) handleAgentResolved(ctx context.Context, inc *db.Incident, result *agent.DiagnosticResult) {
	if result.VerifyAfterSeconds > 0 {
		s.runVerification(ctx, inc, result)
		return
	}
	s.markFixedAndNotify(ctx, inc, result.PrimaryAction)
}

// markFixedAndNotify atomically flips the incident to agent_fixed and DMs reporters
// only if this goroutine actually performed the transition. agent_fixed is excluded
// from the allowed source states, so a second concurrent finisher (e.g. a stale
// verification loop after a reopen) transitions nothing and stays silent — the fix
// for the duplicate "fixed automatically" DM.
func (s *Service) markFixedAndNotify(ctx context.Context, inc *db.Incident, action string) {
	changed, err := s.db.TransitionStatus(ctx, inc.ID, db.StatusAgentFixed,
		db.StatusOpen, db.StatusInvestigating, db.StatusVerifying, db.StatusReopened)
	if err != nil {
		s.log.ErrorContext(ctx, "mark fixed transition", "incident", inc.ID, "error", err)
		return
	}
	if !changed {
		s.log.InfoContext(ctx, "fix already recorded by another run, not re-notifying", "incident", inc.ID)
		return
	}
	s.log.InfoContext(ctx, "agent fixed", "incident", inc.ID, "action", action)
	s.notifyReporters(
		ctx,
		inc,
		fmt.Sprintf("✅ Your report for **%s** has been fixed automatically. Give it a try!", inc.Title),
	)
}

// runVerification re-checks, up to maxVerifyLoops times, whether a deferred
// non-destructive fix (e.g. a library scan) resolved the problem. While checking,
// the incident sits in "verifying". On success it is marked fixed and reporters
// are told. If it never verifies but a scan is still running, reporters get a
// friendly ETA and the incident stays in "verifying" — it is NOT escalated. Only
// when nothing is in progress and it is still broken do we escalate to the owner.
func (s *Service) runVerification(ctx context.Context, inc *db.Incident, result *agent.DiagnosticResult) {
	itemID := result.VerifyItemID
	if itemID == "" {
		itemID = inc.JellyfinItemID
	}

	// Gate entry the same way as every other transition: if a concurrent run has
	// already finished this incident, do not resurrect it into "verifying".
	// StatusManualTestNeeded is included so ApproveEscalation (owner ran a fix
	// from manual_test_needed) can drop into the same re-check loop.
	changed, err := s.db.TransitionStatus(ctx, inc.ID, db.StatusVerifying,
		db.StatusOpen, db.StatusInvestigating, db.StatusReopened, db.StatusManualTestNeeded)
	if err != nil {
		s.log.ErrorContext(ctx, "enter verifying transition", "incident", inc.ID, "error", err)
		return
	}
	if !changed {
		s.log.InfoContext(ctx, "not entering verification, incident already progressed", "incident", inc.ID)
		return
	}

	delay := min(time.Duration(result.VerifyAfterSeconds)*time.Second, verifyLoopDelayCap)

	for range maxVerifyLoops {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		if itemID != "" && s.agent.VerifyResolved(ctx, itemID, result.PreFix) {
			s.markFixedAndNotify(ctx, inc, result.PrimaryAction)
			return
		}
	}

	// Exhausted the verification budget. If a scan is still in progress, the fix
	// is likely still landing — give the reporter an ETA instead of escalating.
	if s.agent.ScanRunning(ctx) {
		eta := result.UserETAMinutes
		if eta <= 0 {
			eta = defaultUserETAMinutes
		}
		s.log.InfoContext(ctx, "fix still applying, advising reporters", "incident", inc.ID, "eta_min", eta)
		s.notifyReporters(ctx, inc, fmt.Sprintf(
			"🔧 We're still rebuilding the library for **%s** — it should be playable in about %d minute(s). Give it a try then!",
			inc.Title,
			eta,
		))
		return
	}

	// Nothing in progress and still broken — escalate to the owner.
	s.log.WarnContext(ctx, "fix not verified, escalating", "incident", inc.ID)
	s.surfaceToOwner(ctx, inc, result, " (autonomous fix applied but could not be verified)")
}

// escalateToOwner atomically marks the incident manual_test_needed and DMs the
// owner, but only if this call actually performs the transition. Every "give up
// and ask a human" path (agent error, control-review escalation, verification
// exhausted, max retries hit) routes through here so escalation is idempotent
// under concurrent/superseded runs — the same TransitionStatus pattern used for
// the "fixed" notification, applied once structurally instead of per call site.
func (s *Service) escalateToOwner(ctx context.Context, inc *db.Incident, msg string) {
	changed, err := s.db.TransitionStatus(ctx, inc.ID, db.StatusManualTestNeeded,
		db.StatusOpen, db.StatusInvestigating, db.StatusVerifying, db.StatusReopened,
		db.StatusBlocked, db.StatusAgentFixed)
	if err != nil {
		s.log.ErrorContext(ctx, "escalate transition", "incident", inc.ID, "error", err)
		return
	}
	if !changed {
		s.log.InfoContext(ctx, "already escalated by another run, not re-notifying owner", "incident", inc.ID)
		return
	}
	_ = s.notif.NotifyOwner(ctx, msg)
}

// handleRunError distinguishes a superseded run from a genuine diagnostic
// failure. A run is superseded either because its context was cancelled by a
// newer rerun (LLM call fails with [context.Canceled]) or because it lost the
// race to claim the incident's status before starting
// (agent.ErrIncidentNotInvestigatable). Either way it exits quietly instead of
// escalating to the owner.
func (s *Service) handleRunError(ctx context.Context, inc *db.Incident, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, agent.ErrIncidentNotInvestigatable) {
		s.log.InfoContext(ctx, "run superseded, exiting quietly", "incident", inc.ID)
		return
	}
	s.handleAgentError(ctx, inc, err)
}

func (s *Service) handleAgentError(ctx context.Context, inc *db.Incident, err error) {
	s.log.ErrorContext(ctx, "agent error", "incident", inc.ID, "error", err)
	s.escalateToOwner(ctx, inc, fmt.Sprintf(
		"❌ Agent error for incident **%s** (#%s): %v\nIncident marked for manual review.",
		inc.Title,
		inc.ID[:8],
		err,
	))
}

func (s *Service) surfaceToOwner(ctx context.Context, inc *db.Incident, result *agent.DiagnosticResult, note string) {
	s.escalateToOwner(ctx, inc, fmt.Sprintf(
		"🔍 Incident **%s** (#%s) needs your attention%s.\nRoot cause: %s\nAction needed: %s\nReason: %s",
		inc.Title, inc.ID[:8], note, result.RootCause, agent.EscalationSummary(result), result.PrimaryReason,
	))
}

// Resolve marks an incident as resolved (called from dashboard or Discord).
func (s *Service) Resolve(ctx context.Context, id string) error {
	if err := s.db.UpdateIncidentStatus(ctx, id, db.StatusResolved); err != nil {
		return err
	}
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	s.notifyReporters(ctx, inc, fmt.Sprintf("✅ Your report for **%s** has been resolved. Give it a try!", inc.Title))
	return nil
}

func (s *Service) notifyReporters(ctx context.Context, inc *db.Incident, msg string) {
	ids, err := s.db.ListDiscordReporterIDs(ctx, inc.ID)
	if err != nil {
		s.log.ErrorContext(ctx, "list discord reporter IDs", "incident", inc.ID, "error", err)
		return
	}
	for _, id := range ids {
		if notifyErr := s.notif.NotifyUser(ctx, id, msg); notifyErr != nil {
			s.log.ErrorContext(ctx, "notify reporter", "user", id, "incident", inc.ID, "error", notifyErr)
			// A failed reporter DM (e.g. "no mutual guilds" — the reporter DM'd the
			// bot from a server it isn't in) used to just vanish into an ERROR log
			// line: the reporter was never told anything, and no one else was
			// either. Fall back to the owner so a silently-undeliverable DM is at
			// least visible somewhere.
			_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
				"⚠️ Could not DM reporter %s for **%s** (#%s): %v\nMessage was: %s",
				id, inc.Title, inc.ID[:8], notifyErr, msg,
			))
		}
	}
}

// Unlock clears an incident's autonomous lock so the agent may act on it again.
// This is a manual owner override (e.g. for a systemic-failure "blocked" incident).
func (s *Service) Unlock(ctx context.Context, id string) error {
	return s.db.SetAutonomousLocked(ctx, id, false)
}

// Rerun re-runs diagnosis for an incident: on the dashboard's "still broken"
// path, on the "Re-investigate" path, and on zombie recovery after a crash.
// These used to be two separate methods (Reopen/Reinvestigate) that did the
// same thing with different seeding, and both had the same bug: neither reset
// status to a state Agent.Run's own transition gate would accept, so
// re-running an already-terminal incident (agent_fixed, manual_test_needed)
// silently did nothing — the run executed but could never transition the
// incident, and runVerification's matching gate then refused to enter
// verification too, logging "not entering verification, incident already
// progressed" with no visible effect on the dashboard.
//
// Rerun fixes that by setting status to Reopened — an allowed source for
// every downstream transition — before launching, and always seeds the run
// with a summary of the prior conversation (if any) so the agent knows what
// it already tried and does not repeat a failed action from scratch.
func (s *Service) Rerun(ctx context.Context, id string) error {
	// Deliberate human/system override — clear any autonomous lock so the
	// re-run actually proceeds, even for a previously blocked/locked incident.
	_ = s.db.SetAutonomousLocked(ctx, id, false)
	if err := s.db.UpdateIncidentStatus(ctx, id, db.StatusReopened); err != nil {
		return err
	}
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	s.launchWithSeed(inc, func(seedCtx context.Context) []openai.ChatCompletionMessage {
		return s.buildReinvestigateSeed(seedCtx, id, inc)
	})
	_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
		"🔁 Incident **%s** (#%s) is being re-run.",
		inc.Title, inc.ID[:8],
	))
	return nil
}

func (s *Service) buildReinvestigateSeed(
	ctx context.Context,
	id string,
	inc *db.Incident,
) []openai.ChatCompletionMessage {
	if s.agent == nil || s.summarizer == nil {
		return nil
	}
	rawConv, loadErr := s.db.LoadConversation(ctx, id)
	if errors.Is(loadErr, db.ErrNotFound) || len(rawConv) == 0 {
		return nil
	}
	if loadErr != nil {
		s.log.WarnContext(
			ctx,
			"load conversation failed, reinvestigating from scratch",
			"incident",
			id,
			"error",
			loadErr,
		)
		return nil
	}
	summary, sumErr := s.summarizer.Summarize(ctx, rawConv)
	if sumErr != nil {
		s.log.WarnContext(ctx, "summarize failed, reinvestigating from scratch", "incident", id, "error", sumErr)
		return nil
	}
	if summary == "" {
		return nil
	}
	s.log.InfoContext(ctx, "reinvestigate with summary seed", "incident", id, "summary_len", len(summary))
	return s.agent.BuildSummarySeed(ctx, inc, summary)
}

// RecoverZombies is called on startup to resume any incidents left in
// "investigating" status from a previous process run — a crashed process
// with nothing left running to check. SweepStaleRuns is the companion for a
// process that never crashed at all: a run whose goroutine is still alive
// but has stopped making progress, which this cannot see (nothing here is
// wrong from the process's point of view).
func (s *Service) RecoverZombies(ctx context.Context) {
	incidents, err := s.db.FindByStatus(ctx, db.StatusInvestigating)
	if err != nil {
		s.log.ErrorContext(ctx, "zombie recovery query", "error", err)
		return
	}
	for _, inc := range incidents {
		s.log.WarnContext(ctx, "recovering zombie incident", "incident", inc.ID, "title", inc.Title)
		if rerunErr := s.Rerun(ctx, inc.ID); rerunErr != nil {
			s.log.ErrorContext(ctx, "zombie rerun failed", "incident", inc.ID, "error", rerunErr)
		}
	}
}

// SweepStaleRuns reruns any incident stuck in "investigating" whose heartbeat
// (see Agent.logTurn/db.TouchHeartbeat) is older than staleRunThreshold — a
// run whose goroutine is still alive but stopped making progress.
func (s *Service) SweepStaleRuns(ctx context.Context) {
	stale, err := s.db.FindStaleInvestigating(ctx, time.Now().Add(-staleRunThreshold))
	if err != nil {
		s.log.ErrorContext(ctx, "stale run sweep query", "error", err)
		return
	}
	for _, inc := range stale {
		s.log.WarnContext(ctx, "recovering stale (hung) run", "incident", inc.ID, "title", inc.Title)
		if rerunErr := s.Rerun(ctx, inc.ID); rerunErr != nil {
			s.log.ErrorContext(ctx, "stale run rerun failed", "incident", inc.ID, "error", rerunErr)
		}
	}
}

// sweepStaleRunsLoop periodically calls SweepStaleRuns until base is done.
// Started once from NewService, tied to the same process-lifetime context
// that owns every other piece of Service's background lifecycle (runManager).
func (s *Service) sweepStaleRunsLoop(base context.Context) {
	ticker := time.NewTicker(staleRunSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-base.Done():
			return
		case <-ticker.C:
			s.SweepStaleRuns(base)
		}
	}
}

// SetAutonomousPaused toggles the global pause switch.
func (s *Service) SetAutonomousPaused(ctx context.Context, paused bool) error {
	v := "false"
	if paused {
		v = "true"
	}
	return s.db.SetSetting(ctx, "autonomous_paused", v)
}
