package server

import (
	"encoding/json"
	"net/http"

	"github.com/minz1/mediafixer/internal/incident"
)

// seerrPayload is the JSON shape we expect from Seerr's outbound webhook.
// Configure Seerr's webhook JSON template to match this structure.
type seerrPayload struct {
	NotificationType string `json:"notification_type"` // e.g. "ISSUE_CREATED"
	Subject          string `json:"subject"`
	Message          string `json:"message"`
	IssueID          string `json:"issue_id"`
	IssueType        string `json:"issue_type"`   // VIDEO | AUDIO | SUBTITLES | OTHER
	IssueStatus      string `json:"issue_status"` // OPEN | RESOLVED
	MediaType        string `json:"media_type"`   // movie | tv
	MediaTmdbID      string `json:"media_tmdbid"`
	MediaJellyfinID  string `json:"media_jellyfinMediaId"`
	ReportedBy       string `json:"reported_by"`
}

func (s *Server) handleSeerrWebhook(w http.ResponseWriter, r *http.Request) {
	var payload seerrPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Only act on new issues; ignore comments/resolves (those come from us).
	if payload.NotificationType != "ISSUE_CREATED" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	what := seerrIssueTypeToWhat(payload.IssueType)
	title := payload.Subject
	if title == "" {
		title = payload.Message
	}

	rep := &incident.Report{
		Source:         "seerr",
		ReportedBy:     payload.ReportedBy,
		What:           what,
		Title:          title,
		JellyfinItemID: payload.MediaJellyfinID,
		Details:        payload.Message,
	}

	inc, err := s.svc.Handle(r.Context(), rep)
	if err != nil {
		s.log.Error("seerr webhook: handle", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.Info("seerr issue ingested", "incident", inc.ID, "title", title)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"incident_id": inc.ID})
}

func seerrIssueTypeToWhat(issueType string) string {
	switch issueType {
	case "VIDEO", "AUDIO", "SUBTITLES":
		return "cant_play"
	case "OTHER":
		return "other"
	default:
		return "other"
	}
}
