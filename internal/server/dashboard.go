package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/livecheck"
)

//go:embed templates/dashboard.html
var templateFS embed.FS

const dashboardPageSize = 50

// tplKeyBaseURL is the template data key every page passes s.baseURL under.
const tplKeyBaseURL = "BaseURL"

// Shared Tailwind badge classes, reused across statusColor, checkColor, and
// confidenceColor.
const (
	badgeGray   = "bg-gray-100 text-gray-600"
	badgeRed    = "bg-red-100 text-red-800"
	badgeYellow = "bg-yellow-100 text-yellow-800"
	badgeGreen  = "bg-green-100 text-green-800"
)

// dashboardTemplates holds the parsed template set.
type dashboardTemplates struct {
	t *template.Template
}

func buildDashboardTemplate() (*dashboardTemplates, error) {
	t, err := template.New("dashboard.html").Funcs(template.FuncMap{
		"json": func(v any) string {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
		// dict builds a map[string]any from alternating key/value pairs, for
		// passing extra data (e.g. a page Title) into {{template "head" ...}}
		// without every page's own top-level data needing a Title field.
		"dict": dictFunc,
		"timeAgo": func(t time.Time) string {
			d := time.Since(t)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
			case d < 24*time.Hour:
				return d.Round(time.Hour).String()
			default:
				return t.Format("Jan 2")
			}
		},
		"statusColor": func(s db.IncidentStatus) string {
			switch s {
			case db.StatusOpen:
				return badgeYellow
			case db.StatusInvestigating:
				return "bg-blue-100 text-blue-800"
			case db.StatusAgentFixed:
				return badgeGreen
			case db.StatusManualTestNeeded:
				return "bg-orange-100 text-orange-800"
			case db.StatusResolved:
				return badgeGray
			case db.StatusReopened:
				return badgeRed
			case db.StatusBlocked:
				return badgeRed
			case db.StatusVerifying:
				return "bg-purple-100 text-purple-800"
			default:
				return badgeGray
			}
		},
		"checkColor": func(s livecheck.Status) string {
			switch s {
			case livecheck.StatusOK:
				return badgeGreen
			case livecheck.StatusDegraded:
				return badgeYellow
			case livecheck.StatusFail, livecheck.StatusMissing:
				return badgeRed
			case livecheck.StatusUnconfigured:
				return "bg-orange-100 text-orange-800"
			case livecheck.StatusSkipped:
				return "bg-gray-100 text-gray-500"
			default:
				return badgeGray
			}
		},
		"confidenceColor": func(confidence string) string {
			switch strings.ToLower(confidence) {
			case "high":
				return badgeGreen
			case "medium":
				return badgeYellow
			case "low":
				return badgeRed
			default:
				return badgeGray
			}
		},
	}).ParseFS(templateFS, "templates/dashboard.html")
	if err != nil {
		return nil, err
	}
	return &dashboardTemplates{t: t}, nil
}

// dictPairSize is how many args make up one dict entry (a key and a value).
const dictPairSize = 2

// dictFunc implements the "dict" template func: alternating key/value pairs
// into a map[string]any. Go's html/template has no builtin equivalent (unlike
// Sprig, which most projects pull this from) — small enough to write inline
// rather than take on a dependency for one helper.
func dictFunc(pairs ...any) (map[string]any, error) {
	if len(pairs)%dictPairSize != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments: %d", len(pairs))
	}
	m := make(map[string]any, len(pairs)/dictPairSize)
	for i := 0; i < len(pairs); i += dictPairSize {
		key, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %d is %T, not string", i, pairs[i])
		}
		m[key] = pairs[i+1]
	}
	return m, nil
}

// incidentIDParam extracts the "id" URL param for the GET routes that render
// an incident (the page, its transcript, its export, and the SSE stream) —
// looser than parseUUIDParam's 400-on-malformed-UUID, matching this
// long-standing handler's original behavior: any lookup miss (malformed or
// simply nonexistent) ends up as the same "not found" from GetIncident.
func incidentIDParam(r *http.Request) string {
	return chi.URLParam(r, "id")
}

// buildIncidentPageData assembles everything an incident's pages/fragments
// render from: the incident row, its action log, any pending async-fix
// outcome, the typed diagnosis (nil if there is none yet or it doesn't
// parse), and the reconstructed conversation grouped into turns. Shared by
// dashboardIncident (the full page), incidentTranscript (the full
// thought-process page), and incidentEvents (the SSE handler re-rendering
// live) so all three ever build this data one way. full controls how much
// of the transcript the template shows — see transcriptTurn's doc comment.
func (s *Server) buildIncidentPageData(ctx context.Context, id string, full bool) (map[string]any, error) {
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		return nil, err
	}
	actions, _ := s.db.ListActions(ctx, id)
	// PendingOutcome is nil (not an error) when the incident has no async
	// fix in flight/escalated — GetPendingOutcome's db.ErrNotFound is exactly
	// that case, so the template's {{if .PendingOutcome}} just sees nothing.
	pendingOutcome, _ := s.db.GetPendingOutcome(ctx, id)

	var turns []transcriptTurn
	if s.journal != nil {
		if conv, convErr := s.journal.Conversation(ctx, id); convErr == nil {
			turns = buildTranscriptTurns(conv)
		}
	}

	return map[string]any{
		"Incident":       inc,
		"Actions":        actions,
		"PendingOutcome": pendingOutcome,
		"Finding":        diagnosticResultOrNil(inc.Finding),
		"Transcript":     turns,
		"Full":           full,
		tplKeyBaseURL:    s.baseURL,
	}, nil
}

func (s *Server) dashboardIndex(w http.ResponseWriter, r *http.Request) {
	paused, _ := s.db.IsAutonomousPaused(r.Context())
	incidents, err := s.db.ListIncidents(r.Context(), "", dashboardPageSize, 0)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.Execute(w, map[string]any{
		"Incidents":   incidents,
		"Paused":      paused,
		tplKeyBaseURL: s.baseURL,
	})
}

func (s *Server) dashboardIncident(w http.ResponseWriter, r *http.Request) {
	id := incidentIDParam(r)
	data, err := s.buildIncidentPageData(r.Context(), id, false)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "incident", data)
}

func (s *Server) actionResolve(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	if err := s.svc.Resolve(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, _ := url.JoinPath(s.baseURL, "incidents", id)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) actionRerun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	if err := s.svc.Rerun(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, _ := url.JoinPath(s.baseURL, "incidents", id)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) actionPause(w http.ResponseWriter, r *http.Request) {
	_ = s.svc.SetAutonomousPaused(r.Context(), true)
	http.Redirect(w, r, s.baseURL+"/", http.StatusSeeOther)
}

func (s *Server) actionResume(w http.ResponseWriter, r *http.Request) {
	_ = s.svc.SetAutonomousPaused(r.Context(), false)
	http.Redirect(w, r, s.baseURL+"/", http.StatusSeeOther)
}

// actionKeepSearching re-arms a pending arr_search_missing outcome that was
// escalated because no release was found yet (see Service.KeepSearching).
func (s *Server) actionKeepSearching(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	if err := s.svc.KeepSearching(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, _ := url.JoinPath(s.baseURL, "incidents", id)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) actionUnlock(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	if err := s.svc.Unlock(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, _ := url.JoinPath(s.baseURL, "incidents", id)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// escalationPreview renders a read-only plan preview for the incident's
// recommended escalation, swapped into the incident page's preview slot. On
// error it renders the error inline at 200 instead of an HTTP error status —
// simpler than branching on swap-on-error behavior at all, and it means this
// handler's behavior didn't need to change for the htmx 2→4 migration (htmx 4
// swaps every non-204/304 response, unlike htmx 2; see selftestRunError for a
// handler that does return real error statuses and has to account for that).
func (s *Server) escalationPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	data := map[string]any{}
	plan, err := s.svc.PreviewEscalation(r.Context(), id)
	if err != nil {
		data["Error"] = err.Error()
	} else {
		data["Plan"] = plan
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "escalation_preview", data)
}

// approveEscalation executes the incident's recommended escalation after
// owner approval.
func (s *Server) approveEscalation(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	if err := s.svc.ApproveEscalation(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, _ := url.JoinPath(s.baseURL, "incidents", id)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// parseUUIDParam extracts the "id" URL param and validates it as a UUID.
func parseUUIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "id")
	parsed, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return "", false
	}
	return parsed.String(), true
}
