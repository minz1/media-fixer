package incident

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/google/uuid"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/journal"
)

const SystematicIncidentThreshold = 5

const (
	// defaultUserETAMinutes is the fallback "try again in N minutes" estimate.
	defaultUserETAMinutes = 10
)

// Service manages incident lifecycle and orchestrates agent runs.
type Service struct {
	db          *db.DB
	journal     *journal.Journal
	agent       AgentRunner
	control     *agent.ControlReviewer
	summarizer  *agent.Summarizer
	notif       Notifier
	seerr       SeerrIssues
	stuckRead   StuckReadChecker
	log         *slog.Logger
	runs        *runManager
	work        *workers
	seedTimeout time.Duration
	unit        atomic.Int64
	timersWarn  atomic.Bool
	// shortcut: in memory, so a restart during a lock sends one more alert; persist it if that turns out noisy.
	lockAlertAt atomic.Int64
	paused      atomic.Bool

	seerrAlertMu sync.Mutex
	seerrAlertAt time.Time
}

type SeerrIssues interface {
	Comment(ctx context.Context, issueID, message string) error
	Resolve(ctx context.Context, issueID string) error
}

// Notifier is implemented by the Discord bot to send DMs.
type Notifier interface {
	NotifyOwner(ctx context.Context, c card.Card) error
	NotifyUser(ctx context.Context, userID string, c card.Card) error
}

// NewService wires up the incident service. base is the process-lifetime context
// (e.g. from signal.NotifyContext); all background agent runs derive from it, so
// shutdown cancels them.
func NewService(
	base context.Context,
	database *db.DB,
	jrnl *journal.Journal,
	ag AgentRunner,
	control *agent.ControlReviewer,
	summarizer *agent.Summarizer,
	notif Notifier,
	log *slog.Logger,
) *Service {
	work := newWorkers(base)
	s := &Service{
		db:         database,
		journal:    jrnl,
		agent:      ag,
		control:    control,
		summarizer: summarizer,
		notif:      notif,
		log:        log,
		runs:       newRunManager(work.base),
		work:       work,
	}
	if paused, err := database.IsAutonomousPaused(base); err == nil {
		s.paused.Store(paused)
	}
	return s
}

func (s *Service) Start() {
	if s.work.goNamed("sweep", s.backgroundSweepLoop) {
		s.log.Info("incident sweep loop started")
	}
}

func (s *Service) StartRecovery() {
	s.work.goNamed("recovery", s.RecoverZombies)
}

func (s *Service) Stop() { s.work.stop() }

func (s *Service) Shutdown(ctx context.Context) error {
	s.Stop()
	if names := s.work.wait(ctx); len(names) > 0 {
		return fmt.Errorf("%w: %s", ErrShutdownTimeout, strings.Join(names, ", "))
	}
	return nil
}

func (s *Service) minutes(n int) time.Duration {
	unit := time.Duration(s.unit.Load())
	if unit <= 0 {
		unit = time.Minute
	}
	return time.Duration(n) * unit
}

func (s *Service) timers(ctx context.Context) Timers {
	t, err := LoadTimers(ctx, s.db)
	switch {
	case err == nil:
		s.timersWarn.Store(false)
	case ctx.Err() == nil && !s.timersWarn.Swap(true):
		s.log.WarnContext(ctx, "incident timers unreadable, using defaults", "error", err)
	}
	return t
}

func (s *Service) SetSeerr(seerr SeerrIssues) {
	s.seerr = seerr
}

// StuckReadChecker reports whether a title's stuck reads have cleared.
type StuckReadChecker interface{ Clear(title string) bool }

const stuckReadVerifyFloorMinutes = 3

func (s *Service) SetStuckRead(c StuckReadChecker) {
	s.stuckRead = c
}

// launch starts a background agent run for an incident with a fixed seed (nil
// for a fresh run).
func (s *Service) launch(inc *db.Incident, seed []openai.ChatCompletionMessage) {
	ctx, tok, _ := s.runs.supersede(inc.ID)
	s.startRun(ctx, tok, inc, inc.Generation, func(context.Context) []openai.ChatCompletionMessage { return seed })
}

// startRun runs the background agent pipeline for an incident under a token
// from runs.supersede, which cancels any in-flight run for the same incident, so a
// rerun supersedes a prior run instead of racing it. buildSeed runs on the new
// run's own (background) context rather than the caller's, so a seed built from
// an expensive call (buildReinvestigateSeed summarizes the prior conversation
// via an LLM call) never blocks — or gets silently dropped by — an HTTP request
// context.
func (s *Service) startRun(
	ctx context.Context,
	tok *runToken,
	inc *db.Incident,
	gen int64,
	buildSeed func(context.Context) []openai.ChatCompletionMessage,
) {
	started := s.work.goNamed("run:"+inc.ShortID(), func(context.Context) {
		defer s.runs.end(inc.ID, tok)
		t := s.timers(ctx)
		s.runs.setStaleFloor(tok, t.RetryFirstMinutes+t.RetrySecondMinutes+staleMargin)
		seedTimeout := s.seedTimeout
		if seedTimeout == 0 {
			seedTimeout = s.minutes(t.StaleRunMinutes)
		}
		seedCtx, cancel := context.WithTimeout(ctx, seedTimeout)
		seed := buildSeed(seedCtx)
		cancel()
		s.runAgent(ctx, inc, seed, t, s.gateFor(inc.ID, gen))
	})
	if !started {
		s.runs.end(inc.ID, tok)
		s.log.InfoContext(ctx, "run not started, shutting down", "incident", inc.ID)
	}
}

// Report is the normalised form of an incoming issue report from any source.
type Report struct {
	Source            string // "discord" | "seerr"
	ReportedBy        string
	ReporterDiscordID string // empty for non-Discord sources
	SeerrIssueID      string
	What              string // "cant_play" | "buffering" | "login_failed" | "missing_media" | "other"
	Title             string
	JellyfinItemID    string
	Details           string
}

// Handle processes a new report: deduplicates, creates/updates the incident,
// and starts the agent in the background if this is a new incident.
func (s *Service) Handle(ctx context.Context, r *Report) (*db.Incident, error) {
	nr := &db.NewIncident{
		ID:                uuid.New().String(),
		Source:            r.Source,
		ReportedBy:        r.ReportedBy,
		ReporterDiscordID: r.ReporterDiscordID,
		SeerrIssueID:      r.SeerrIssueID,
		What:              r.What,
		Title:             r.Title,
		JellyfinItemID:    r.JellyfinItemID,
		Details:           r.Details,
	}
	release := s.runs.reserve(nr.ID)
	defer release()
	t := s.timers(ctx)
	a, err := s.db.CreateFromReport(ctx, nr, s.decide(lifecycle.Event{Kind: lifecycle.Reported}, s.limits(t), nil))
	if err != nil {
		return nil, fmt.Errorf("record report: %w", err)
	}
	if a.Merged {
		s.log.InfoContext(ctx, "duplicate report collapsed", "incident", a.Snap.ID, "reporter", r.ReportedBy)
	} else if a.Snap.Status != lifecycle.Blocked {
		s.lockAlertAt.Store(0)
	}
	s.afterCommit(ctx, a, t, r.SeerrIssueID)
	return s.db.GetIncident(ctx, a.Snap.ID)
}

// runDiagnosis executes one agent.Run call under the process-wide diagnostic
// lock (runManager.globalSlot), so a concurrent incident's restart/scan/cleanup
// can never be observed mid-disruption by this run's evidence gathering — see
// runManager's doc comment. Deliberately does NOT wrap runVerification or
// escalation's durable outcome tracking; only the ~10s tool-calling loop itself
// is serialized.
func (s *Service) runDiagnosis(
	ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage, gate *runGate,
) agent.RunExit {
	if err := s.runs.acquireGlobal(ctx); err != nil {
		return agent.RunExit{State: agent.RunSuperseded, Err: err}
	}
	exit := func() agent.RunExit {
		defer s.runs.releaseGlobal()
		return s.agent.Run(ctx, inc, seed, gate)
	}()
	if exit.Started {
		s.attemptFinished(ctx, gate, exit)
	}
	return exit
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

	proposal := controlProposal(result, riskReason, s.actionHistorySummary(ctx, inc.ID))
	verdict, verdictErr := s.control.Review(ctx, conversation, proposal)
	if verdictErr != nil && ctx.Err() != nil {
		return diagnosisOutcome{done: true}
	}
	if verdictErr != nil {
		// Fail closed. This branch used to apply the fix anyway when the model
		// hadn't itself asked for approval — but the only reason review was
		// running is that reviewRiskReason flagged it (low confidence, a
		// repeat of something that already failed, or several actions already
		// tried). Those are precisely the proposals that must not be
		// auto-approved because the reviewer was unreachable or returned
		// something unparseable.
		s.log.ErrorContext(ctx, "control review error", "incident", inc.ID, "error", verdictErr)
		if result.RequiresApproval {
			s.surfaceToOwner(ctx, inc, result, " (control review failed)")
		} else {
			s.surfaceToOwner(ctx, inc, result, fmt.Sprintf(
				" (control review failed — flagged because %s)", riskReason))
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
		s.escalate(ctx, inc, lifecycle.ReasonNeedsOwner, ownerCard(inc, card.Warn, fmt.Sprintf(
			"⚠️ Control reviewer flagged a potentially unreliable diagnosis for **%s** (#%s).",
			inc.Title,
			inc.ShortID(),
		), "", card.Field{Name: fieldRootCause, Value: result.RootCause}, card.Field{Name: "Concern", Value: verdict.Reason}))
		return diagnosisOutcome{done: true}

	case agent.VerdictSuggestAlternative:
		if attempt == maxAttempts-1 {
			s.escalate(ctx, inc, lifecycle.ReasonNeedsOwner, ownerCard(inc, card.Warn, fmt.Sprintf(
				"⚠️ **%s** (#%s): agent still needs approval after %d retries.",
				inc.Title, inc.ShortID(), attempt+1,
			), "",
				card.Field{Name: fieldRootCause, Value: result.RootCause},
				card.Field{Name: fieldAction, Value: agent.EscalationSummary(result)}))
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
	// Catches ladder-climbing that actionAlreadyTried can't: each rung is a
	// *new* action (clear cache, then scan, then restart...), so the exact
	// same-action check above never fires even though the pattern — working
	// down a priority list rather than following new evidence — is exactly
	// what the systemPrompt tells the model not to do. This doesn't try to
	// judge whether THIS proposal is evidenced; it just makes sure a human
	// reviewer looks once the incident has already burned through a couple
	// of distinct fixes.
	if n := s.distinctAppliedActionCount(ctx, incidentID); n >= ladderClimbActionThreshold {
		return fmt.Sprintf(
			"%d different actions have already been tried on this incident without resolving it, and this "+
				"would be another new one — worth confirming it's grounded in evidence from this run rather "+
				"than a mechanical walk down the priority list",
			n,
		)
	}
	return ""
}

// ladderClimbActionThreshold is the number of distinct already-applied
// actions on an incident that triggers control review for any further new
// action, regardless of the model's own confidence.
const ladderClimbActionThreshold = 2

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

// distinctAppliedActionCount counts how many different action names have
// been successfully applied on this incident (not how many times — three
// applications of the same action count as one).
func (s *Service) distinctAppliedActionCount(ctx context.Context, incidentID string) int {
	actions, err := s.db.ListActions(ctx, incidentID)
	if err != nil {
		return 0
	}
	seen := make(map[string]bool)
	for _, act := range actions {
		if act.Status == db.ActionApplied {
			seen[act.Action] = true
		}
	}
	return len(seen)
}

// actionHistorySummary renders the distinct actions already applied on this
// incident, for the control reviewer's context — it needs to see the pattern
// it's being asked to judge (see distinctAppliedActionCount's ladder-climbing
// trigger above), not just this one proposal in isolation.
func (s *Service) actionHistorySummary(ctx context.Context, incidentID string) string {
	actions, err := s.db.ListActions(ctx, incidentID)
	if err != nil || len(actions) == 0 {
		return ""
	}
	var applied []string
	for _, act := range actions {
		if act.Status == db.ActionApplied {
			applied = append(applied, act.Action)
		}
	}
	if len(applied) == 0 {
		return ""
	}
	return "\nActions already applied on this incident (none resolved it): " + strings.Join(applied, ", ")
}

// controlProposal builds the description of what's being reviewed and why,
// passed to ControlReviewer.Review — see controlSystemPrompt in
// internal/agent/control.go for how the two trigger cases are meant to read.
func controlProposal(result *agent.DiagnosticResult, riskReason, actionHistory string) string {
	if result.RequiresApproval {
		return fmt.Sprintf(
			"The agent wants to escalate the following action for owner approval:\n\n%s%s",
			agent.EscalationSummary(result), actionHistory,
		)
	}
	return fmt.Sprintf(
		"The agent proposes to autonomously apply %q (not flagged for owner approval), but this "+
			"review was triggered because %s.\n\nRoot cause: %s\nReason: %s%s",
		result.PrimaryAction, riskReason, result.RootCause, result.PrimaryReason, actionHistory,
	)
}

// legacyFixed atomically flips the incident to agent_fixed and DMs reporters
// only if this goroutine actually performed the transition. agent_fixed is excluded
// from the allowed source states, so a second concurrent finisher (e.g. a stale
// verification loop after a reopen) transitions nothing and stays silent — the fix
// for the duplicate "fixed automatically" DM.
func (s *Service) legacyFixed(ctx context.Context, inc *db.Incident, action string) {
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
	s.recordStatusChanged(ctx, inc.ID, string(db.StatusAgentFixed))
	s.log.InfoContext(ctx, "agent fixed", "incident", inc.ID, "action", action)
	s.notifyReporters(
		ctx,
		inc,
		reporterCard(
			card.Success,
			fmt.Sprintf("✅ Your report for **%s** has been fixed automatically. Give it a try!", inc.Title),
		),
	)
	s.resolveSeerrIssues(ctx, inc)
}

// legacyVerification re-checks, up to the configured number of times, whether a deferred
// non-destructive fix (e.g. a library scan) resolved the problem. While checking,
// the incident sits in "verifying". On success it is marked fixed and reporters
// are told. If it never verifies but a scan is still running, reporters get a
// friendly ETA and the incident stays in "verifying" — it is NOT escalated. Only
// when nothing is in progress and it is still broken do we escalate to the owner.
func (s *Service) legacyVerification(ctx context.Context, inc *db.Incident, result *agent.DiagnosticResult) {
	// Same precedence as handleCompleteDiagnosis: the incident's own item
	// wins, so verification can't be pointed at a different item than the
	// pre-fix baseline was captured against.
	itemID := inc.JellyfinItemID
	if itemID == "" {
		itemID = result.VerifyItemID
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
	s.recordStatusChanged(ctx, inc.ID, string(db.StatusVerifying))

	t := s.timers(ctx)
	firstDelay := min(time.Duration(result.VerifyAfterSeconds)*time.Second, s.minutes(t.FirstVerifyCapMinutes))
	stuckRead := inc.What == agent.WhatStuckRead
	if stuckRead {
		firstDelay = max(firstDelay, s.minutes(stuckReadVerifyFloorMinutes))
	}

	for i := range t.VerifyChecks {
		delay := s.minutes(t.VerifyGapMinutes)
		if i == 0 {
			delay = firstDelay
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		var fixed bool
		if stuckRead {
			fixed = s.stuckRead != nil && s.stuckRead.Clear(inc.Title)
		} else {
			fixed = s.agent.VerifyResolved(ctx, itemID, inc.Title, result.PreFix)
		}
		if fixed {
			s.legacyFixed(ctx, inc, result.PrimaryAction)
			return
		}
	}

	// Exhausted the verification budget. If a scan is still in progress, the fix
	// is likely still landing — give the reporter an ETA instead of escalating.
	if !stuckRead && s.agent.ScanRunning(ctx) {
		eta := result.UserETAMinutes
		if eta <= 0 {
			eta = defaultUserETAMinutes
		}
		s.log.InfoContext(ctx, "fix still applying, advising reporters", "incident", inc.ID, "eta_min", eta)
		s.notifyReporters(ctx, inc, reporterCard(card.Info, fmt.Sprintf(
			"🔧 We're still rebuilding the library for **%s** — it should be playable in about %d minute(s). Give it a try then!",
			inc.Title,
			eta,
		)))
		return
	}

	// Nothing in progress and still broken — escalate to the owner.
	s.log.WarnContext(ctx, "fix not verified, escalating", "incident", inc.ID)
	s.legacyEscalate(ctx, inc, surfaceCard(inc, result, " (autonomous fix applied but could not be verified)"))
}

// legacyEscalate atomically marks the incident manual_test_needed and DMs the
// owner, but only if this call actually performs the transition. Every "give up
// and ask a human" path (agent error, control-review escalation, verification
// exhausted, max retries hit) routes through here so escalation is idempotent
// under concurrent/superseded runs — the same TransitionStatus pattern used for
// the "fixed" notification, applied once structurally instead of per call site.
func (s *Service) legacyEscalate(ctx context.Context, inc *db.Incident, c card.Card) {
	prior, getErr := s.db.GetIncident(ctx, inc.ID)
	if getErr != nil {
		s.log.ErrorContext(ctx, "read status before escalation", "incident", inc.ID, "error", getErr)
	}
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
	s.recordStatusChanged(ctx, inc.ID, string(db.StatusManualTestNeeded))
	c.Subject, c.Verbs = card.IncidentSubject(inc.ID), escalationVerbs()
	_ = s.notif.NotifyOwner(ctx, c)
	if getErr == nil && prior.Status != db.StatusAgentFixed {
		s.notifyReporters(ctx, inc,
			reporterCard(card.Info, fmt.Sprintf("🧑‍🔧 Handed **%s** to minz1. You'll hear back here.", inc.Title)))
	}
}

var errRunNotExited = errors.New("the agent run returned without an exit")

func (s *Service) notifyReporters(ctx context.Context, inc *db.Incident, c card.Card) {
	s.claimReporterNote(ctx, inc.ID, time.Now(), 0)
	s.deliverToReporters(ctx, inc, c)
}

func (s *Service) noteReporters(
	ctx context.Context,
	inc *db.Incident,
	c card.Card,
	now time.Time,
	minGap time.Duration,
) {
	if s.claimReporterNote(ctx, inc.ID, now, minGap) {
		s.deliverToReporters(ctx, inc, c)
	}
}

func (s *Service) claimReporterNote(ctx context.Context, id string, now time.Time, minGap time.Duration) bool {
	claimed, err := s.db.ClaimReporterNote(ctx, id, now, minGap)
	if err != nil {
		s.log.ErrorContext(ctx, "claim reporter note", "incident", id, "error", err)
	}
	return claimed
}

func (s *Service) deliverToReporters(ctx context.Context, inc *db.Incident, c card.Card) {
	s.notifyDiscordReporters(ctx, inc, c)
	s.notifySeerrIssues(ctx, inc, "comment on", func(id string) error { return s.seerr.Comment(ctx, id, c.Plain()) })
}

func (s *Service) notifyDiscordReporters(ctx context.Context, inc *db.Incident, c card.Card) {
	ids, err := s.db.ListDiscordReporterIDs(ctx, inc.ID)
	if err != nil {
		s.log.ErrorContext(ctx, "list discord reporter IDs", "incident", inc.ID, "error", err)
		return
	}
	for _, id := range ids {
		if notifyErr := s.notif.NotifyUser(ctx, id, c); notifyErr != nil {
			s.log.ErrorContext(ctx, "notify reporter", "user", id, "incident", inc.ID, "error", notifyErr)
			// A failed reporter DM (e.g. "no mutual guilds" — the reporter DM'd the
			// bot from a server it isn't in) used to just vanish into an ERROR log
			// line: the reporter was never told anything, and no one else was
			// either. Fall back to the owner so a silently-undeliverable DM is at
			// least visible somewhere.
			_ = s.notif.NotifyOwner(ctx, ownerCard(inc, card.Warn, fmt.Sprintf(
				"⚠️ Could not DM reporter %s for **%s** (#%s): %v",
				id, inc.Title, inc.ShortID(), notifyErr,
			), "", card.Field{Name: "Message was", Value: c.Plain()}))
		}
	}
}

func (s *Service) notifySeerrIssues(ctx context.Context, inc *db.Incident, verb string, call func(string) error) {
	if s.seerr == nil {
		return
	}
	ids, err := s.db.ListSeerrIssueIDs(ctx, inc.ID)
	if err != nil {
		s.log.ErrorContext(ctx, "list seerr issue IDs", "incident", inc.ID, "error", err)
		return
	}
	for _, id := range ids {
		s.callSeerrIssue(ctx, inc, verb, id, call)
	}
}

func (s *Service) callSeerrIssue(ctx context.Context, inc *db.Incident, verb, id string, call func(string) error) {
	if err := call(id); err != nil {
		s.log.ErrorContext(ctx, "seerr "+verb, "issue", id, "incident", inc.ID, "error", err)
		now := time.Now()
		t := s.timers(ctx)
		prev, ok := s.claimSeerrAlert(now, s.minutes(t.SeerrAlertGapMinutes))
		if !ok {
			return
		}
		if derr := s.notif.NotifyOwner(ctx, ownerCard(inc, card.Warn, fmt.Sprintf(
			"⚠️ Could not %s Seerr issue %s for **%s** (#%s): %v",
			verb, id, inc.Title, inc.ShortID(), err,
		), "Further Seerr failures in the next "+gapText(t.SeerrAlertGapMinutes)+" are only logged.")); derr != nil {
			s.log.ErrorContext(ctx, "seerr alert DM failed", "incident", inc.ID, "error", derr)
			s.releaseSeerrAlert(now, prev)
		}
	}
}

func gapText(minutes int) string {
	if minutes%minutesPerHour != 0 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	if h := minutes / minutesPerHour; h != 1 {
		return fmt.Sprintf("%d hours", h)
	}
	return "hour"
}

func (s *Service) claimSeerrAlert(now time.Time, gap time.Duration) (time.Time, bool) {
	s.seerrAlertMu.Lock()
	defer s.seerrAlertMu.Unlock()
	prev := s.seerrAlertAt
	if now.Sub(prev) < gap {
		return prev, false
	}
	s.seerrAlertAt = now
	return prev, true
}

func (s *Service) releaseSeerrAlert(claimed, prev time.Time) {
	s.seerrAlertMu.Lock()
	defer s.seerrAlertMu.Unlock()
	if s.seerrAlertAt.Equal(claimed) {
		s.seerrAlertAt = prev
	}
}

func (s *Service) resolveSeerrIssues(ctx context.Context, inc *db.Incident) {
	s.notifySeerrIssues(ctx, inc, "resolve", func(id string) error { return s.seerr.Resolve(ctx, id) })
}

// recordStatusChanged appends a status_changed event so an incident page's
// live SSE connection re-renders and re-checks incidentSettled instead of
// only updating on the next event the agent loop happens to emit — every
// status transition this service performs (Resolve, Rerun, the verification/
// escalation lifecycle, Unlock) happens here, one layer above Agent.Run's own
// event recording, so without this call the live page would never see its
// own final status: Agent.Run's last event (run_finished) fires before
// Service.handleAgentResolved/legacyEscalate ever transitions status away
// from "investigating".
func (s *Service) recordStatusChanged(ctx context.Context, incidentID, status string) {
	if s.journal == nil {
		return
	}
	if err := s.journal.StatusChanged(ctx, incidentID, status); err != nil {
		s.log.WarnContext(ctx, "record status_changed", "incident", incidentID, "error", err)
	}
}

type rerunOrigin int

const (
	rerunManual rerunOrigin = iota
	rerunRestart
	rerunStale
)

func (s *Service) rerun(ctx context.Context, id string, origin rerunOrigin, staleMinutes int) error {
	leave, err := s.ownerSection("rerun", id)
	if err != nil {
		return err
	}
	defer leave()
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	runCtx, tok, replaced := s.runs.supersede(id)
	// Deliberate human/system override — clear any autonomous lock so the
	// re-run actually proceeds, even for a previously blocked/locked incident.
	if lockErr := s.db.SetAutonomousLocked(ctx, id, false); lockErr == nil {
		inc.AutonomousLocked = false
	}
	if err = s.db.UpdateIncidentStatus(ctx, id, db.StatusReopened); err != nil {
		s.runs.end(id, tok)
		if replaced {
			s.log.ErrorContext(ctx, "rerun cancelled the running run and started no replacement",
				"incident", id, "error", err)
		}
		return err
	}
	inc.Status = db.StatusReopened
	s.clearEscalationPlan(ctx, id)
	s.recordStatusChanged(ctx, id, string(db.StatusReopened))
	switch origin {
	case rerunManual:
		s.noteReporters(
			ctx,
			inc,
			reporterCard(
				card.Info,
				fmt.Sprintf("🔁 Taking another look at **%s**.", inc.Title),
			),
			time.Now(),
			s.minutes(s.timers(ctx).RerunNoteGapMinutes),
		)
	case rerunStale:
		c := ownerCard(inc, card.Info, fmt.Sprintf(
			"⏱️ Incident **%s** (#%s) stopped making progress for %d min and is being re-run.",
			inc.Title, inc.ShortID(), staleMinutes,
		), "")
		c.Subject, c.Verbs = card.IncidentSubject(inc.ID), []string{verbResolve}
		_ = s.notif.NotifyOwner(ctx, c)
	case rerunRestart:
	}
	s.startRun(runCtx, tok, inc, inc.Generation, func(seedCtx context.Context) []openai.ChatCompletionMessage {
		return s.buildReinvestigateSeed(seedCtx, id, inc)
	})
	return nil
}

func (s *Service) buildReinvestigateSeed(
	ctx context.Context,
	id string,
	inc *db.Incident,
) []openai.ChatCompletionMessage {
	if s.agent == nil || s.summarizer == nil || s.journal == nil {
		return nil
	}
	conv, loadErr := s.journal.Conversation(ctx, id)
	if errors.Is(loadErr, db.ErrNotFound) || len(conv) == 0 {
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
	summary, sumErr := s.summarizer.Summarize(ctx, conv)
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
// "investigating" or "reopened" status, or a stuck_read incident left in
// "verifying", from a previous process run — a crashed process
// with nothing left running to check. SweepStaleRuns is the companion for a
// process that never crashed at all: a run whose goroutine is still alive
// but has stopped making progress, which this cannot see (nothing here is
// wrong from the process's point of view).
func (s *Service) RecoverZombies(ctx context.Context) {
	for _, status := range []db.IncidentStatus{db.StatusInvestigating, db.StatusReopened} {
		incidents, err := s.db.FindByStatus(ctx, status)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.ErrorContext(ctx, "zombie recovery query", "status", status, "error", err)
			continue
		}
		for _, inc := range incidents {
			if ctx.Err() != nil {
				return
			}
			s.log.WarnContext(ctx, "recovering zombie incident", "incident", inc.ID, "status", status)
			if rerunErr := s.rerun(ctx, inc.ID, rerunRestart, 0); rerunErr != nil && ctx.Err() == nil {
				s.logRerunErr(ctx, "zombie rerun failed", inc.ID, rerunErr)
			}
		}
	}
	s.recoverOpen(ctx)
	s.armDueChecks(ctx)
}

func (s *Service) recoverOpen(ctx context.Context) {
	incidents, err := s.db.FindByStatus(ctx, db.StatusOpen)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "open incident recovery query", "error", err)
		}
		return
	}
	if len(incidents) == 0 {
		return
	}
	paused, err := s.db.IsAutonomousPaused(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.ErrorContext(ctx, "open incident recovery: pause check", "error", err)
		}
		return
	}
	if paused {
		s.log.InfoContext(ctx, "open incident recovery skipped, autonomous actions paused", "incidents", len(incidents))
		return
	}
	for _, inc := range incidents {
		if ctx.Err() != nil {
			return
		}
		if inc.AutonomousLocked || s.runs.running(inc.ID) {
			continue
		}
		s.log.InfoContext(ctx, "starting open incident left without a run", "incident", inc.ID)
		s.launch(inc, nil)
	}
}

// SweepStaleRuns reruns any incident stuck in "investigating" whose heartbeat
// (see Agent.logTurn/db.TouchHeartbeat) is older than the saved stale-run minutes — a
// run whose goroutine is still alive but stopped making progress.
func (s *Service) SweepStaleRuns(ctx context.Context) {
	staleMinutes := s.timers(ctx).StaleRunMinutes
	now := time.Now()
	staleBefore := now.Add(-s.minutes(staleMinutes))
	stale, err := s.db.FindStaleInvestigating(ctx, staleBefore)
	if err != nil {
		s.log.ErrorContext(ctx, "stale run sweep query", "error", err)
	}
	for _, inc := range stale {
		minutes := max(staleMinutes, s.runs.staleFloor(inc.ID))
		if minutes > staleMinutes && !s.staleSince(ctx, inc.ID, now.Add(-s.minutes(minutes))) {
			continue
		}
		s.log.WarnContext(ctx, "recovering stale (hung) run", "incident", inc.ID, "title", inc.Title)
		if rerunErr := s.rerun(ctx, inc.ID, rerunStale, minutes); rerunErr != nil {
			s.logRerunErr(ctx, "stale run rerun failed", inc.ID, rerunErr)
		}
	}
	s.sweepStaleReopened(ctx, staleBefore, staleMinutes)
}

func (s *Service) staleSince(ctx context.Context, id string, before time.Time) bool {
	stale, err := s.db.FindStaleInvestigating(ctx, before)
	if err != nil {
		s.log.ErrorContext(ctx, "stale run sweep query", "error", err)
		return false
	}
	return slices.ContainsFunc(stale, func(inc *db.Incident) bool { return inc.ID == id })
}

func (s *Service) sweepStaleReopened(ctx context.Context, staleBefore time.Time, staleMinutes int) {
	paused, err := s.db.IsAutonomousPaused(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "stale reopened sweep: pause check", "error", err)
		return
	}
	if paused {
		return
	}
	stale, err := s.db.FindStaleReopened(ctx, staleBefore)
	if err != nil {
		s.log.ErrorContext(ctx, "stale reopened sweep query", "error", err)
		return
	}
	for _, inc := range stale {
		if s.runs.running(inc.ID) {
			continue
		}
		s.log.WarnContext(ctx, "recovering stale reopened incident", "incident", inc.ID, "title", inc.Title)
		if rerunErr := s.rerun(ctx, inc.ID, rerunStale, staleMinutes); rerunErr != nil {
			s.logRerunErr(ctx, "stale reopened rerun failed", inc.ID, rerunErr)
		}
	}
}

func (s *Service) backgroundSweepLoop(base context.Context) {
	d := s.minutes(s.timers(base).CheckEveryMinutes)
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-base.Done():
			return
		case <-ticker.C:
			s.SweepStaleRuns(base)
			s.armDueChecks(base)
			s.AdvanceReporterProgress(base, time.Now())
			s.endLockEpisode(base)
			if next := s.minutes(s.timers(base).CheckEveryMinutes); next != d {
				ticker.Reset(next)
				d = next
			}
		}
	}
}

// endLockEpisode re-arms the lock alert once under the threshold, since reopen and rerun raise the count without Handle.
func (s *Service) endLockEpisode(ctx context.Context) {
	prev := s.lockAlertAt.Load()
	if prev == 0 {
		return
	}
	n, err := s.db.CountOpenIncidents(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "count active incidents", "error", err)
		return
	}
	if n < SystematicIncidentThreshold {
		s.lockAlertAt.CompareAndSwap(prev, 0)
	}
}

// SetAutonomousPaused toggles the global pause switch.
func (s *Service) SetAutonomousPaused(ctx context.Context, paused bool) error {
	leave, err := s.ownerSection("pause", "")
	if err != nil {
		return err
	}
	defer leave()
	v := "false"
	if paused {
		v = "true"
	}
	if err = s.db.SetSetting(ctx, "autonomous_paused", v); err != nil {
		return err
	}
	s.paused.Store(paused)
	if s.journal != nil {
		s.journal.AutonomousPauseChanged()
	}
	return nil
}
