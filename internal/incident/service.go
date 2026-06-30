package incident

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Service manages incident lifecycle and orchestrates agent runs.
type Service struct {
	db         *db.DB
	agent      *agent.Agent
	control    *agent.ControlReviewer
	summarizer *agent.Summarizer
	notif      Notifier
	log        *slog.Logger
}

// Notifier is implemented by the Discord bot to send DMs.
type Notifier interface {
	NotifyOwner(ctx context.Context, msg string) error
	NotifyUser(ctx context.Context, userID, msg string) error
}

// NewService wires up the incident service.
func NewService(
	database *db.DB,
	ag *agent.Agent,
	control *agent.ControlReviewer,
	summarizer *agent.Summarizer,
	notif Notifier,
	log *slog.Logger,
) *Service {
	return &Service{db: database, agent: ag, control: control, summarizer: summarizer, notif: notif, log: log}
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

	go s.runAgent(context.WithoutCancel(ctx), inc, nil)

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

	var (
		result       *agent.DiagnosticResult
		conversation []openai.ChatCompletionMessage
	)

	for attempt, delay := range retryDelays {
		if delay > 0 {
			time.Sleep(delay)
		}

		result, conversation, err = s.agent.Run(ctx, inc, seed)
		if err != nil {
			s.handleAgentError(ctx, inc, err)
			return
		}

		if !result.RequiresApproval {
			s.handleAgentResolved(ctx, inc, result)
			return
		}

		if s.control == nil {
			s.surfaceToOwner(ctx, inc, result, "")
			return
		}

		verdict, verdictErr := s.control.Review(ctx, conversation, result.EscalateAction)
		if verdictErr != nil {
			s.log.ErrorContext(ctx, "control review error", "incident", inc.ID, "error", verdictErr)
			s.surfaceToOwner(ctx, inc, result, " (control review failed)")
			return
		}

		switch verdict.Verdict {
		case agent.VerdictApprove:
			s.surfaceToOwner(ctx, inc, result, "")
			return

		case agent.VerdictEscalateToOwner:
			_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
			_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
				"⚠️ Control reviewer flagged a potentially unreliable diagnosis for **%s** (#%s).\nRoot cause: %s\nConcern: %s",
				inc.Title,
				inc.ID[:8],
				result.RootCause,
				verdict.Reason,
			))
			return

		case agent.VerdictSuggestAlternative:
			if attempt == len(retryDelays)-1 {
				_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
				_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
					"⚠️ **%s** (#%s): agent still needs approval after %d retries.\nRoot cause: %s\nAction needed: %s",
					inc.Title, inc.ID[:8], attempt+1, result.RootCause, result.EscalateAction,
				))
				return
			}
			feedback := openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleUser,
				Content: fmt.Sprintf(
					"Control review suggests an alternative approach: %s\n\nPlease re-evaluate and try this approach before escalating.",
					verdict.AlternativeAction,
				),
			}
			conversation = append(conversation, feedback)
			seed = conversation
			s.log.InfoContext(ctx, "control reviewer suggested alternative, retrying",
				"incident", inc.ID,
				"attempt", attempt+1,
				"alternative", verdict.AlternativeAction,
			)
		}
	}
}

// handleAgentResolved processes a non-approval diagnosis: either it kicks off the
// verification loop (when the fix needs time) or marks the incident fixed now.
func (s *Service) handleAgentResolved(ctx context.Context, inc *db.Incident, result *agent.DiagnosticResult) {
	if result.VerifyAfterSeconds > 0 {
		s.runVerification(ctx, inc, result)
		return
	}
	_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusAgentFixed)
	s.log.InfoContext(ctx, "agent fixed", "incident", inc.ID, "action", result.PrimaryAction)
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

	_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusVerifying)

	delay := min(time.Duration(result.VerifyAfterSeconds)*time.Second, verifyLoopDelayCap)

	for range maxVerifyLoops {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		if itemID != "" && s.agent.VerifyResolved(ctx, itemID) {
			_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusAgentFixed)
			s.log.InfoContext(ctx, "fix verified", "incident", inc.ID, "action", result.PrimaryAction)
			s.notifyReporters(
				ctx,
				inc,
				fmt.Sprintf("✅ Your report for **%s** has been fixed automatically. Give it a try!", inc.Title),
			)
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

func (s *Service) handleAgentError(ctx context.Context, inc *db.Incident, err error) {
	s.log.ErrorContext(ctx, "agent error", "incident", inc.ID, "error", err)
	_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
	_ = s.notif.NotifyOwner(
		ctx,
		fmt.Sprintf(
			"❌ Agent error for incident **%s** (#%s): %v\nIncident marked for manual review.",
			inc.Title,
			inc.ID[:8],
			err,
		),
	)
}

func (s *Service) surfaceToOwner(ctx context.Context, inc *db.Incident, result *agent.DiagnosticResult, note string) {
	_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
	_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
		"🔍 Incident **%s** (#%s) needs your attention%s.\nRoot cause: %s\nAction needed: %s",
		inc.Title, inc.ID[:8], note, result.RootCause, result.EscalateAction,
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
		}
	}
}

// Unlock clears an incident's autonomous lock so the agent may act on it again.
// This is a manual owner override (e.g. for a systemic-failure "blocked" incident).
func (s *Service) Unlock(ctx context.Context, id string) error {
	return s.db.SetAutonomousLocked(ctx, id, false)
}

// Reopen marks an incident as reopened (when human testing shows it's still broken).
func (s *Service) Reopen(ctx context.Context, id string) error {
	// Reopening is a deliberate human override — clear any autonomous lock so the
	// re-run actually proceeds, even for a previously blocked/locked incident.
	_ = s.db.SetAutonomousLocked(ctx, id, false)
	if err := s.db.UpdateIncidentStatus(ctx, id, db.StatusReopened); err != nil {
		return err
	}
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	go s.runAgent(context.WithoutCancel(ctx), inc, nil)
	_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
		"🔁 Incident **%s** (#%s) was reopened — re-running diagnostics.",
		inc.Title, inc.ID[:8],
	))
	return nil
}

// Reinvestigate resumes a stuck or failed incident by summarizing the prior
// conversation (if any) and spawning a fresh agent run seeded with the summary.
func (s *Service) Reinvestigate(ctx context.Context, id string) error {
	// Deliberate human override — clear any autonomous lock so the re-run proceeds.
	_ = s.db.SetAutonomousLocked(ctx, id, false)
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}

	seed := s.buildReinvestigateSeed(ctx, id, inc)
	go s.runAgent(context.WithoutCancel(ctx), inc, seed)
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
	return s.agent.BuildSummarySeed(inc, summary)
}

// RecoverZombies is called on startup to resume any incidents left in
// "investigating" status from a previous process run.
func (s *Service) RecoverZombies(ctx context.Context) {
	incidents, err := s.db.FindByStatus(ctx, db.StatusInvestigating)
	if err != nil {
		s.log.ErrorContext(ctx, "zombie recovery query", "error", err)
		return
	}
	for _, inc := range incidents {
		s.log.WarnContext(ctx, "recovering zombie incident", "incident", inc.ID, "title", inc.Title)
		if reinvestErr := s.Reinvestigate(ctx, inc.ID); reinvestErr != nil {
			s.log.ErrorContext(ctx, "zombie reinvestigate failed", "incident", inc.ID, "error", reinvestErr)
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
