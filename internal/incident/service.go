package incident

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/minz1/mediafixer/internal/opexec"
)

const SystematicIncidentThreshold = 5

// Service manages incident lifecycle and orchestrates agent runs.
type Service struct {
	db          *db.DB
	journal     *journal.Journal
	agent       AgentRunner
	exec        *opexec.Executor
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
	wake        chan struct{}

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
		exec:       &opexec.Executor{Store: database},
		control:    control,
		summarizer: summarizer,
		notif:      notif,
		log:        log,
		runs:       newRunManager(work.base),
		wake:       make(chan struct{}, 1),
		work:       work,
	}
	if paused, err := database.IsAutonomousPaused(base); err == nil {
		s.paused.Store(paused)
	}
	return s
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

// startRun runs the background agent pipeline for an incident under a token
// from runs.supersedeGen, which cancels any in-flight run for the same incident, so a
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
	gen int64,
	result *agent.DiagnosticResult,
	conversation []openai.ChatCompletionMessage,
	attempt, maxAttempts int,
) diagnosisOutcome {
	owner, err := needsOwner(result.Outcome.Kind)
	if err != nil {
		s.handleAgentError(ctx, inc, gen, err)
		return diagnosisOutcome{done: true}
	}
	riskReason := s.reviewRiskReason(ctx, inc.ID, result)
	if !owner && riskReason == "" {
		s.handleAgentResolved(ctx, inc, gen, result)
		return diagnosisOutcome{done: true}
	}

	if s.control == nil {
		if owner {
			s.surfaceToOwner(ctx, inc, gen, result, "")
		} else {
			// No reviewer configured to double-check a repeat/low-confidence
			// proposal the model didn't itself flag — proceed rather than block
			// on a review that can never happen.
			s.handleAgentResolved(ctx, inc, gen, result)
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
		if owner {
			s.surfaceToOwner(ctx, inc, gen, result, " (control review failed)")
		} else {
			s.surfaceToOwner(ctx, inc, gen, result, fmt.Sprintf(
				" (control review failed — flagged because %s)", riskReason))
		}
		return diagnosisOutcome{done: true}
	}

	switch verdict.Verdict {
	case agent.VerdictApprove:
		if owner {
			s.surfaceToOwner(ctx, inc, gen, result, "")
		} else {
			// The reviewer approved a fix the model didn't itself flag for
			// approval (this review was triggered by our own repeat/low-
			// confidence heuristic) — let it proceed autonomously as proposed.
			s.handleAgentResolved(ctx, inc, gen, result)
		}
		return diagnosisOutcome{done: true}

	case agent.VerdictEscalateToOwner:
		s.escalate(ctx, inc, gen, lifecycle.ReasonNeedsOwner, ownerCard(inc, card.Warn, fmt.Sprintf(
			"⚠️ Control reviewer flagged a potentially unreliable diagnosis for **%s** (#%s).",
			inc.Title,
			inc.ShortID(),
		), "", card.Field{Name: fieldRootCause, Value: result.RootCause}, card.Field{Name: "Concern", Value: verdict.Reason}))
		return diagnosisOutcome{done: true}

	case agent.VerdictSuggestAlternative:
		if attempt == maxAttempts-1 {
			s.escalate(ctx, inc, gen, lifecycle.ReasonNeedsOwner, ownerCard(inc, card.Warn, fmt.Sprintf(
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
// to the outcome kind in evaluateDiagnosis.
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
	if owner, _ := needsOwner(result.Outcome.Kind); owner {
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

var errRunNotExited = errors.New("the agent run returned without an exit")

var errUnclassified = errors.New("the diagnosis has no outcome")

func needsOwner(k agent.DiagnosisKind) (bool, error) {
	switch k {
	case agent.DiagnosisNeedsApproval, agent.DiagnosisManual:
		return true, nil
	case agent.DiagnosisFixNow, agent.DiagnosisDeferredVerify, agent.DiagnosisAwaitDownload:
		return false, nil
	}
	return false, fmt.Errorf("%w: kind %q", errUnclassified, k)
}

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
	if !paused {
		s.wakeReconciler()
	}
	if s.journal != nil {
		s.journal.AutonomousPauseChanged()
	}
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

func (s *Service) SetOpPorts(p opexec.Ports) {
	s.exec.Ports = p
}
