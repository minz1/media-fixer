package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

// exportFilenameTimeFormat avoids RFC3339's colons, which are awkward-to-
// invalid in a downloaded filename on some filesystems (notably Windows).
const exportFilenameTimeFormat = "20060102-150405"

// incidentExportBundle is the full JSON export for one incident: everything
// db knows about it plus its complete raw event log. The event log — not a
// hand-picked bundle of six separate queries — is deliberately the
// centerpiece: it's the same data source powering the live transcript, so
// what you download is exactly what you were watching, not an approximation
// of it. This is also the direct replacement for the old workflow of
// hand-copying agent_turn lines out of Loki to debug a run after the fact.
type incidentExportBundle struct {
	ExportedAt     string             `json:"exported_at"`
	Incident       *db.Incident       `json:"incident"`
	Reporters      []string           `json:"reporters"`
	Actions        []*db.ActionLog    `json:"actions"`
	PendingOutcome *db.PendingOutcome `json:"pending_outcome,omitempty"`
	Events         []*db.Event        `json:"events"`
}

// incidentExport downloads the full export bundle as a JSON file attachment.
func (s *Server) incidentExport(w http.ResponseWriter, r *http.Request) {
	id := incidentIDParam(r)
	ctx := r.Context()

	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	reporters, _ := s.db.ListReporters(ctx, id)
	actions, _ := s.db.ListActions(ctx, id)
	pendingOutcome, poErr := s.db.GetPendingOutcome(ctx, id)
	if errors.Is(poErr, db.ErrNotFound) {
		pendingOutcome = nil
	}
	var events []*db.Event
	if s.journal != nil {
		events, _ = s.journal.Since(ctx, id, 0)
	}

	now := time.Now()
	bundle := incidentExportBundle{
		ExportedAt:     now.Format(time.RFC3339),
		Incident:       inc,
		Reporters:      reporters,
		Actions:        actions,
		PendingOutcome: pendingOutcome,
		Events:         events,
	}

	filename := fmt.Sprintf("incident-%s-%s.json", shortID(id), now.Format(exportFilenameTimeFormat))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(bundle)
}

// shortID returns the first 8 characters of a UUID for compact display, or
// the whole string if it's shorter (defensive — every real incident ID is a
// full UUID, but a test fixture might not be). Same convention as
// Agent.shortID (internal/agent/agent.go), duplicated rather than exported
// across the package boundary for one eight-character slice.
func shortID(id string) string {
	const shortLen = 8
	if len(id) <= shortLen {
		return id
	}
	return id[:shortLen]
}
