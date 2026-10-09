package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"

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

// maxSeerrBodyBytes bounds the webhook body. The endpoint is reachable by
// anything that can reach the port, and an unbounded [json.Decoder] will buffer
// whatever it is given.
const maxSeerrBodyBytes = 1 << 20 // 1 MiB

func (s *Server) handleSeerrWebhook(w http.ResponseWriter, r *http.Request) {
	if s.seerrWebhookSecret == "" {
		http.Error(w, "seerr webhook not configured", http.StatusServiceUnavailable)
		return
	}
	want := []byte("Bearer " + s.seerrWebhookSecret)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var payload seerrPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSeerrBodyBytes)).Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Only act on new issues; ignore comments/resolves (those come from us).
	if payload.NotificationType != "ISSUE_CREATED" && payload.NotificationType != "ISSUE_REOPENED" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	what := seerrIssueTypeToWhat(payload.IssueType)
	title := payload.Subject
	if title == "" {
		title = payload.Message
	}

	details := payload.Message
	if payload.MediaType != "" {
		details = "[media_type:" + payload.MediaType + "] " + details
	}

	issueID := payload.IssueID
	if _, err := strconv.ParseUint(issueID, 10, 64); err != nil {
		issueID = ""
	}

	rep := &incident.Report{
		Source:         "seerr",
		ReportedBy:     payload.ReportedBy,
		What:           what,
		Title:          title,
		JellyfinItemID: payload.MediaJellyfinID,
		Details:        details,
		SeerrIssueID:   issueID,
	}

	inc, err := s.svc.Handle(r.Context(), rep)
	if err != nil {
		s.log.ErrorContext(r.Context(), "seerr webhook: handle", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.log.InfoContext(r.Context(), "seerr issue ingested", "incident", inc.ID, "title", title)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if encErr := json.NewEncoder(w).Encode(map[string]string{"incident_id": inc.ID}); encErr != nil {
		s.log.ErrorContext(r.Context(), "encode response", "error", encErr)
	}
}

func seerrIssueTypeToWhat(issueType string) string {
	switch issueType {
	case "VIDEO", "AUDIO", "SUBTITLES":
		return "cant_play"
	default:
		return "other"
	}
}
