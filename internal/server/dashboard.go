package server

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/minz1/mediafixer/internal/db"
)

var dashboardTmpl = template.Must(template.New("dashboard").Funcs(template.FuncMap{
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
}).Parse(`
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Media Fixer</title>
  <script src="https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js"></script>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="bg-gray-50 font-sans">
  <div class="max-w-5xl mx-auto px-4 py-8">
    <div class="flex items-center justify-between mb-8">
      <h1 class="text-2xl font-bold text-gray-900">Media Fixer</h1>
      <div class="flex items-center gap-3">
        {{if .Paused}}
        <span class="text-sm font-medium text-red-600">⏸ Autonomous actions paused</span>
        <button hx-post="{{.BaseURL}}/resume" hx-target="body" hx-swap="outerHTML"
                class="px-3 py-1.5 text-sm bg-green-600 text-white rounded hover:bg-green-700">
          Resume
        </button>
        {{else}}
        <button hx-post="{{.BaseURL}}/pause" hx-target="body" hx-swap="outerHTML"
                class="px-3 py-1.5 text-sm bg-red-600 text-white rounded hover:bg-red-700">
          Pause agent
        </button>
        {{end}}
      </div>
    </div>

    <div class="space-y-3">
      {{range .Incidents}}
      <a href="{{$.BaseURL}}/incidents/{{.ID}}"
         class="block bg-white border border-gray-200 rounded-lg p-4 hover:shadow-sm transition-shadow">
        <div class="flex items-start justify-between">
          <div>
            <div class="font-medium text-gray-900">{{.Title}}</div>
            <div class="text-sm text-gray-500 mt-0.5">
              {{.What}} · {{.Source}} · {{timeAgo .CreatedAt}}
            </div>
          </div>
          <span class="ml-4 shrink-0 px-2 py-0.5 text-xs font-medium rounded-full {{statusColor .Status}}">
            {{.Status}}
          </span>
        </div>
        {{if .AutonomousLocked}}
        <div class="mt-2 text-xs text-red-600">🔒 Autonomous actions locked</div>
        {{end}}
      </a>
      {{else}}
      <div class="text-center py-16 text-gray-400">No incidents</div>
      {{end}}
    </div>
  </div>
</body>
</html>

{{define "incident"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Incident {{.Incident.ID}} · Media Fixer</title>
  <script src="https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js"></script>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="bg-gray-50 font-sans">
  <div class="max-w-4xl mx-auto px-4 py-8">
    <a href="{{.BaseURL}}/" class="text-sm text-blue-600 hover:underline">← All incidents</a>

    <div class="mt-4 bg-white border border-gray-200 rounded-lg p-6">
      <div class="flex items-start justify-between">
        <div>
          <h2 class="text-xl font-bold text-gray-900">{{.Incident.Title}}</h2>
          <p class="text-sm text-gray-500 mt-1">
            #{{.Incident.ID}} · {{.Incident.Source}} · reported by {{.Incident.ReportedBy}}
          </p>
        </div>
        <span class="px-2 py-0.5 text-xs font-medium rounded-full {{statusColor .Incident.Status}}">
          {{.Incident.Status}}
        </span>
      </div>

      {{if .Incident.Details}}
      <p class="mt-4 text-sm text-gray-700">{{.Incident.Details}}</p>
      {{end}}

      {{if .Incident.Finding}}
      <div class="mt-6">
        <h3 class="text-sm font-semibold text-gray-700 mb-2">Agent diagnosis</h3>
        <pre class="bg-gray-50 border border-gray-200 rounded p-3 text-xs overflow-auto">{{json .Incident.Finding}}</pre>
      </div>
      {{end}}

      <div class="mt-6 flex gap-3">
        {{if eq .Incident.Status "agent_fixed"}}
        <button hx-post="{{.BaseURL}}/incidents/{{.Incident.ID}}/resolve"
                hx-swap="outerHTML" hx-target="body"
                class="px-4 py-2 text-sm bg-green-600 text-white rounded hover:bg-green-700">
          ✓ I tested it — resolved
        </button>
        <button hx-post="{{.BaseURL}}/incidents/{{.Incident.ID}}/reopen"
                hx-swap="outerHTML" hx-target="body"
                class="px-4 py-2 text-sm bg-red-600 text-white rounded hover:bg-red-700">
          ✗ Still broken
        </button>
        {{else if eq .Incident.Status "manual_test_needed"}}
        <button hx-post="{{.BaseURL}}/incidents/{{.Incident.ID}}/reinvestigate"
                hx-swap="outerHTML" hx-target="body"
                class="px-4 py-2 text-sm bg-blue-600 text-white rounded hover:bg-blue-700">
          Re-investigate
        </button>
        <button hx-post="{{.BaseURL}}/incidents/{{.Incident.ID}}/resolve"
                hx-swap="outerHTML" hx-target="body"
                class="px-4 py-2 text-sm bg-green-600 text-white rounded hover:bg-green-700">
          Mark resolved
        </button>
        <button hx-post="{{.BaseURL}}/incidents/{{.Incident.ID}}/reopen"
                hx-swap="outerHTML" hx-target="body"
                class="px-4 py-2 text-sm bg-red-600 text-white rounded hover:bg-red-700">
          Still broken — re-run agent
        </button>
        {{else if eq .Incident.Status "resolved"}}
        <button hx-post="{{.BaseURL}}/incidents/{{.Incident.ID}}/reopen"
                hx-swap="outerHTML" hx-target="body"
                class="px-4 py-2 text-sm bg-orange-600 text-white rounded hover:bg-orange-700">
          Reopen
        </button>
        {{end}}
      </div>
    </div>

    <div class="mt-6">
      <h3 class="text-sm font-semibold text-gray-700 mb-3">Action log</h3>
      <div class="space-y-2">
        {{range .Actions}}
        <div class="bg-white border border-gray-200 rounded p-3 text-sm">
          <div class="flex justify-between">
            <span class="font-medium text-gray-800">{{.Action}}</span>
            <span class="text-xs text-gray-400">{{.TriggeredBy}} · {{.Status}}</span>
          </div>
          {{if .Result}}<div class="mt-1 text-xs text-gray-600">{{.Result}}</div>{{end}}
          {{if .Error}}<div class="mt-1 text-xs text-red-600">{{.Error}}</div>{{end}}
        </div>
        {{else}}
        <div class="text-sm text-gray-400">No actions yet</div>
        {{end}}
      </div>
    </div>
  </div>
</body>
</html>
{{end}}
`))

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
