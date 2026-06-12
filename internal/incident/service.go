package incident

import (
	"context"
	"fmt"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/rs/zerolog"
)

const systematicIncidentThreshold = 5

// Service manages incident lifecycle and orchestrates agent runs.
type Service struct {
	db    *db.DB
	agent *agent.Agent
	notif Notifier
	log   zerolog.Logger
}

// Notifier is implemented by the Discord bot to send DMs.
type Notifier interface {
	NotifyOwner(ctx context.Context, msg string) error
}

func NewService(database *db.DB, ag *agent.Agent, notif Notifier, log zerolog.Logger) *Service {
	return &Service{db: database, agent: ag, notif: notif, log: log}
}

// Report is the normalised form of an incoming issue report from any source.
type Report struct {
	Source         string // "discord" | "seerr"
	ReportedBy     string
	What           string // "cant_play" | "login_failed" | "missing_media" | "other"
	Title          string
	JellyfinItemID string
	Details        string
}

// Handle processes a new report: deduplicates, creates/updates the incident,
// and starts the agent in the background if this is a new incident.
func (s *Service) Handle(ctx context.Context, r *Report) (*db.Incident, error) {
	// Duplicate detection: collapse into existing open incident by title.
	existing, err := s.db.FindOpenByTitle(ctx, r.Title)
	if err != nil {
		return nil, fmt.Errorf("find open incident: %w", err)
	}
	if existing != nil {
		s.log.Info().Str("incident", existing.ID).Str("reporter", r.ReportedBy).Msg("duplicate report collapsed")
		_ = s.db.AddReporter(ctx, existing.ID, r.ReportedBy, r.Source)
		return existing, nil
	}

	// Systemic incident guard: if too many open incidents, lock autonomous
	// actions and page the owner immediately instead of running the agent.
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
	_ = s.db.AddReporter(ctx, inc.ID, r.ReportedBy, r.Source)

	if openCount >= systematicIncidentThreshold {
		_ = s.db.SetAutonomousLocked(ctx, inc.ID, true)
		msg := fmt.Sprintf("⚠️ %d open incidents — possible systemic failure. Autonomous actions locked. New incident: **%s** (#%s)",
			openCount+1, r.Title, inc.ID[:8])
		if err := s.notif.NotifyOwner(ctx, msg); err != nil {
			s.log.Error().Err(err).Msg("notify owner")
		}
		return inc, nil
	}

	// Run the agent asynchronously so the ingest endpoint can return quickly.
	go s.runAgent(inc)

	return inc, nil
}

func (s *Service) runAgent(inc *db.Incident) {
	if s.agent == nil {
		return
	}
	ctx := context.Background()

	paused, err := s.db.IsAutonomousPaused(ctx)
	if err != nil || paused {
		s.log.Warn().Str("incident", inc.ID).Msg("autonomous actions paused, skipping agent")
		return
	}

	result, err := s.agent.Run(ctx, inc)
	if err != nil {
		s.log.Error().Err(err).Str("incident", inc.ID).Msg("agent error")
		_ = s.notif.NotifyOwner(ctx,
			fmt.Sprintf("❌ Agent error for incident **%s** (#%s): %v", inc.Title, inc.ID[:8], err))
		return
	}

	if result.RequiresApproval {
		_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
		_ = s.notif.NotifyOwner(ctx, fmt.Sprintf(
			"🔍 Incident **%s** (#%s) needs your attention.\nRoot cause: %s\nAction needed: %s",
			inc.Title, inc.ID[:8], result.RootCause, result.EscalateAction,
		))
		return
	}

	_ = s.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusAgentFixed)
	s.log.Info().Str("incident", inc.ID).Str("action", result.PrimaryAction).Msg("agent fixed")
}

// Resolve marks an incident as resolved (called from dashboard or Discord).
func (s *Service) Resolve(ctx context.Context, id string) error {
	return s.db.UpdateIncidentStatus(ctx, id, db.StatusResolved)
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
	go s.runAgent(inc)
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
