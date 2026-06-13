package server

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minz1/mediafixer/internal/db"
)

//go:embed templates/dashboard.html
var templateFS embed.FS

var dashboardTmpl = template.Must(
	template.New("dashboard.html").Funcs(template.FuncMap{
		"json": func(v any) string {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
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
				return "bg-yellow-100 text-yellow-800"
			case db.StatusInvestigating:
				return "bg-blue-100 text-blue-800"
			case db.StatusAgentFixed:
				return "bg-green-100 text-green-800"
			case db.StatusManualTestNeeded:
				return "bg-orange-100 text-orange-800"
			case db.StatusResolved:
				return "bg-gray-100 text-gray-600"
			case db.StatusReopened:
				return "bg-red-100 text-red-800"
			default:
				return "bg-gray-100 text-gray-600"
			}
		},
	}).ParseFS(templateFS, "templates/dashboard.html"),
)

func (s *Server) dashboardIndex(w http.ResponseWriter, r *http.Request) {
	paused, _ := s.db.IsAutonomousPaused(r.Context())
	incidents, err := s.db.ListIncidents(r.Context(), "", 50, 0)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboardTmpl.Execute(w, map[string]any{
		"Incidents": incidents,
		"Paused":    paused,
		"BaseURL":   s.baseURL,
	})
}

func (s *Server) dashboardIncident(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	inc, err := s.db.GetIncident(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	actions, _ := s.db.ListActions(r.Context(), id)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = dashboardTmpl.ExecuteTemplate(w, "incident", map[string]any{
		"Incident": inc,
		"Actions":  actions,
		"BaseURL":  s.baseURL,
	})
}

func (s *Server) actionResolve(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.svc.Resolve(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.baseURL+"/incidents/"+id, http.StatusSeeOther)
}

func (s *Server) actionReinvestigate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.svc.Reinvestigate(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.baseURL+"/incidents/"+id, http.StatusSeeOther)
}

func (s *Server) actionReopen(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.svc.Reopen(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.baseURL+"/incidents/"+id, http.StatusSeeOther)
}

func (s *Server) actionPause(w http.ResponseWriter, r *http.Request) {
	_ = s.svc.SetAutonomousPaused(r.Context(), true)
	http.Redirect(w, r, s.baseURL+"/", http.StatusSeeOther)
}

func (s *Server) actionResume(w http.ResponseWriter, r *http.Request) {
	_ = s.svc.SetAutonomousPaused(r.Context(), false)
	http.Redirect(w, r, s.baseURL+"/", http.StatusSeeOther)
}
