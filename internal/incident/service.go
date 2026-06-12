package incident

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	openai "github.com/sashabaranov/go-openai"
)

const systematicIncidentThreshold = 5

// Service manages incident lifecycle and orchestrates agent runs.
type Service struct {
	db      *db.DB
	agent   *agent.Agent
	control *agent.ControlReviewer
	notif   Notifier
	log     *slog.Logger
}

// Notifier is implemented by the Discord bot to send DMs.
type Notifier interface {
	NotifyOwner(ctx context.Context, msg string) error
	NotifyUser(ctx context.Context, userID, msg string) error
}

func NewService(database *db.DB, ag *agent.Agent, control *agent.ControlReviewer, notif Notifier, log *slog.Logger) *Service {
	return &Service{db: database, agent: ag, control: control, notif: notif, log: log}
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
	if err != nil {
		return nil, fmt.Errorf("find open incident: %w", err)
	}
	if existing != nil {
		s.log.Info("duplicate report collapsed", "incident", existing.ID, "reporter", r.ReportedBy)
		_ = s.db.AddReporter(ctx, existing.ID, r.ReportedBy, r.Source, r.ReporterDiscordID)
		return existing, nil
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
	if err := s.db.CreateIncident(ctx, inc); err != nil {
		return nil, fmt.Errorf("create incident: %w", err)
	}
	_ = s.db.AddReporter(ctx, inc.ID, r.ReportedBy, r.Source, r.ReporterDiscordID)

	if openCount >= systematicIncidentThreshold {
		_ = s.db.SetAutonomousLocked(ctx, inc.ID, true)
		msg := fmt.Sprintf("⚠️ %d open incidents — possible systemic failure. Autonomous actions locked. New incident: **%s** (#%s)",
			openCount+1, r.Title, inc.ID[:8])
		if err := s.notif.NotifyOwner(ctx, msg); err != nil {
			s.log.Error("notify owner", "error", err)
		}
		return inc, nil
	}

	go s.runAgent(inc, nil)

	return inc, nil
}

// retryDelays are the backoff waits for suggest_alternative retries (0, 2min, 6min).
var retryDelays = []time.Duration{0, 2 * time.Minute, 6 * time.Minute}

func (s *Service) runAgent(inc *db.Incident, seed []openai.ChatCompletionMessage) {
	if s.agent == nil {
		return
	}
	ctx := context.Background()

	paused, err := s.db.IsAutonomousPaused(ctx)
	if err != nil || paused {
		s.log.Warn("autonomous actions paused, skipping agent", "incident", inc.ID)
		return
	}

	var (
		result       *agent.DiagnosticResult
		conversation []openai.ChatCompletionMessage
	)

	for attempt := 0; attempt < len(retryDelays); attempt++ {
		if retryDelays[attempt] > 0 {
			time.Sleep(retryDelays[attempt])
		}

		result, conversation, err = s.agent.Run(ctx, inc, seed)
		if err != nil {
			s.log.Error("agent error", "incident", inc.ID, "error", err)
			_ = s.notif.NotifyOwner(ctx,
				fmt.Sprintf("❌ Agent error for incident **%s** (#%s): %v", inc.Title, inc.ID[:8], err))
			return
		}

		if !result.RequiresApproval {
			_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusAgentFixed)
			s.log.Info("agent fixed", "incident", inc.ID, "action", result.PrimaryAction)
			s.notifyReporters(ctx, inc, fmt.Sprintf("✅ Your report for **%s** has been fixed automatically. Give it a try!", inc.Title))
			return
		}

		if s.control == nil {
			s.surfaceToOwner(ctx, inc, result, "")
			return
		}

		verdict, err := s.control.Review(ctx, conversation, result.EscalateAction)
		if err != nil {
			s.log.Error("control review error", "incident", inc.ID, "error", err)
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
				inc.Title, inc.ID[:8], result.RootCause, verdict.Reason,
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
			seed = append(conversation, openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("Control review suggests an alternative approach: %s\n\nPlease re-evaluate and try this approach before escalating.", verdict.AlternativeAction),
			})
			s.log.Info("control reviewer suggested alternative, retrying",
				"incident", inc.ID,
				"attempt", attempt+1,
				"alternative", verdict.AlternativeAction,
			)
		}
	}
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
		s.log.Error("list discord reporter IDs", "incident", inc.ID, "error", err)
		return
	}
	for _, id := range ids {
		if err := s.notif.NotifyUser(ctx, id, msg); err != nil {
			s.log.Error("notify reporter", "user", id, "incident", inc.ID, "error", err)
		}
	}
}

// Reopen marks an incident as reopened (when human testing shows it's still broken).
func (s *Service) Reopen(ctx context.Context, id string) error {
	if err := s.db.UpdateIncidentStatus(ctx, id, db.StatusReopened); err != nil {
		return err
	}
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	go s.runAgent(inc, nil)
	_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
		"🔁 Incident **%s** (#%s) was reopened — re-running diagnostics.",
		inc.Title, inc.ID[:8],
	))
	return nil
}

// SetAutonomousPaused toggles the global pause switch.
func (s *Service) SetAutonomousPaused(ctx context.Context, paused bool) error {
	v := "false"
	if paused {
		v = "true"
	}
	return s.db.SetSetting(ctx, "autonomous_paused", v)
}
