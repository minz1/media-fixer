package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/livecheck"
	"github.com/minz1/mediafixer/internal/sweep"
)

//go:embed templates/dashboard.html
var templateFS embed.FS

const dashboardPageSize = 50

// tplKeyBaseURL is the template data key every page passes s.baseURL under.
const (
	tplKeyBaseURL  = "BaseURL"
	tplKeyNotice   = "Notice"
	tplKeyFailure  = "Failure"
	tplKeyPaused   = "Paused"
	msgSaved       = "Saved."
	msgWholeNumber = "must be a whole number"
	doneRestarting = "restarting"
)

const (
	toneGray   = "gray"
	toneRed    = "red"
	toneYellow = "yellow"
	toneGreen  = "green"
	toneBlue   = "blue"
	toneOrange = "orange"
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
		"dict":           dictFunc,
		"size":           humanSize,
		"trigger":        sweep.TriggerLabel,
		"utc":            utcStamp,
		"statusTone":     statusTone,
		"statusWord":     func(st db.IncidentStatus) string { return strings.ReplaceAll(string(st), "_", " ") },
		"checkTone":      checkTone,
		"sweepTone":      sweepTone,
		"season":         seasonLabel,
		"ms":             lookupDuration,
		"confidenceTone": confidenceTone,
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
			turns = buildTranscriptTurns(conv, incidentLive(inc.Status))
		}
	}

	nextStep := 1
	if len(turns) > 0 {
		nextStep = turns[len(turns)-1].Round + 1
	}
	shown, hidden := condenseTurns(turns, full)
	paused := s.isPaused(ctx)

	timers, _ := incident.LoadTimers(ctx, s.db)

	return map[string]any{
		"KeepSearchingSpan": "up to " + incident.KeepSearchingSpan(timers.KeepSearchingDays),
		"Incident":          inc,
		"Actions":           actions,
		"PendingOutcome":    pendingOutcome,
		"Finding":           diagnosticResultOrNil(inc.Finding),
		"Transcript":        shown,
		"EarlierSteps":      hidden,
		"Turns":             len(turns),
		"NextStep":          nextStep,
		"Live":              incidentLive(inc.Status) && !paused,
		"Full":              full,
		tplKeyPaused:        paused,
		tplKeyBaseURL:       s.baseURL,
	}, nil
}

func (s *Server) isPaused(ctx context.Context) bool {
	paused, _ := s.db.IsAutonomousPaused(ctx)
	return paused
}

func incidentDoneNotice(key string) string {
	switch key {
	case "resolve":
		return "Marked resolved."
	case "rerun":
		return "Rerun started."
	case "keep":
		return "Keep searching started."
	case "unlock":
		return "Unlocked."
	case "approve":
		return "Escalation approved; running it now."
	case doneRestarting:
		return card.RestartingNotice
	}
	return ""
}

func restartingNotice(notice string, err error) (string, string) {
	switch {
	case errors.Is(err, card.ErrRestarting):
		return card.RestartingNotice, ""
	case err != nil:
		return "", err.Error()
	}
	return notice, ""
}

func (s *Server) finishIncidentAction(w http.ResponseWriter, r *http.Request, id, done string, err error) {
	switch {
	case errors.Is(err, card.ErrRestarting):
		s.redirectIncident(w, r, id, doneRestarting)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		s.redirectIncident(w, r, id, done)
	}
}

func (s *Server) redirectIncident(w http.ResponseWriter, r *http.Request, id, done string) {
	target, _ := url.JoinPath(s.baseURL, "incidents", id)
	http.Redirect(w, r, target+"?done="+done, http.StatusSeeOther)
}

func (s *Server) dashboardIndex(w http.ResponseWriter, r *http.Request) {
	incidents, err := s.db.ListIncidents(r.Context(), "", dashboardPageSize, 0)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{
		"Incidents":   incidents,
		tplKeyPaused:  s.isPaused(r.Context()),
		tplKeyBaseURL: s.baseURL,
	}
	if r.URL.Query().Get("done") == doneRestarting {
		data["Done"] = card.RestartingNotice
	}
	_ = s.tmpl.t.Execute(w, data)
}

func (s *Server) dashboardIncident(w http.ResponseWriter, r *http.Request) {
	id := incidentIDParam(r)
	data, err := s.buildIncidentPageData(r.Context(), id, false)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	data["Done"] = incidentDoneNotice(r.URL.Query().Get("done"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "incident", data)
}

func (s *Server) actionResolve(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	err := s.svc.Resolve(context.WithoutCancel(r.Context()), id)
	if errors.Is(err, incident.ErrAlreadyResolved) {
		err = nil
	}
	s.finishIncidentAction(w, r, id, "resolve", err)
}

func (s *Server) actionRerun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	s.finishIncidentAction(w, r, id, "rerun", s.svc.Rerun(context.WithoutCancel(r.Context()), id))
}

func (s *Server) actionPause(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, true, "Autonomous actions paused.")
}

func (s *Server) actionResume(w http.ResponseWriter, r *http.Request) {
	s.setPaused(w, r, false, "Autonomous actions resumed.")
}

func (s *Server) setPaused(w http.ResponseWriter, r *http.Request, paused bool, notice string) {
	err := s.svc.SetAutonomousPaused(r.Context(), paused)
	if errors.Is(err, card.ErrRestarting) {
		notice = card.RestartingNotice
	} else if err != nil {
		s.log.ErrorContext(r.Context(), "set autonomous paused", "paused", paused, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("Hx-Request") == "" {
		target := s.baseURL + "/"
		if errors.Is(err, card.ErrRestarting) {
			target += "?done=" + doneRestarting
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "pause_response", map[string]any{
		tplKeyBaseURL: s.baseURL, tplKeyPaused: s.isPaused(r.Context()), tplKeyNotice: notice,
	})
}

// actionKeepSearching re-arms a pending arr_search_missing outcome that was
// escalated because no release was found yet (see Service.KeepSearching).
func (s *Server) actionKeepSearching(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	s.finishIncidentAction(w, r, id, "keep", s.svc.KeepSearching(context.WithoutCancel(r.Context()), id))
}

func (s *Server) actionUnlock(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	s.finishIncidentAction(w, r, id, "unlock", s.svc.Unlock(r.Context(), id))
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
	data := map[string]any{"BaseURL": s.baseURL, "IncidentID": id}
	plan, fp, err := s.svc.PreviewEscalation(r.Context(), id)
	switch {
	case errors.Is(err, card.ErrRestarting):
		data["Error"] = card.RestartingNotice
	case err != nil:
		data["Error"] = err.Error()
	default:
		data["Plan"] = plan
		data["PlanFP"] = fp
		// Surfaced out of the plan rather than reached into from the template:
		// the preview renders several plan types and only one of them has a
		// readability probe, so a template field access would error on the
		// others.
		if rp, isReplace := plan.(*client.ReplacePlan); isReplace && rp.Readability != nil {
			data["ReadableFile"] = rp.Readability.ReadableFile
			data["ReadabilityUnknown"] = !rp.Readability.Checked
			if inc, incErr := s.db.GetIncident(r.Context(), id); incErr == nil && inc.What == agent.WhatStuckRead {
				data["StuckRead"] = true
			}
			data["Reason"] = rp.Reason
		}
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
	err := s.svc.ApproveEscalation(context.WithoutCancel(r.Context()), id, r.FormValue("plan_fp"))
	s.finishIncidentAction(w, r, id, "approve", err)
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

func statusTone(s db.IncidentStatus) string {
	switch s {
	case db.StatusOpen:
		return toneYellow
	case db.StatusInvestigating:
		return toneBlue
	case db.StatusAgentFixed:
		return toneGreen
	case db.StatusManualTestNeeded:
		return toneOrange
	case db.StatusReopened, db.StatusBlocked:
		return toneRed
	case db.StatusVerifying:
		return "purple"
	case db.StatusResolved:
		return toneGray
	default:
		return toneGray
	}
}

func checkTone(s livecheck.Status) string {
	switch s {
	case livecheck.StatusOK:
		return toneGreen
	case livecheck.StatusDegraded:
		return toneYellow
	case livecheck.StatusFail, livecheck.StatusMissing:
		return toneRed
	case livecheck.StatusUnconfigured:
		return toneOrange
	case livecheck.StatusSkipped:
		return toneGray
	default:
		return toneGray
	}
}

func confidenceTone(confidence string) string {
	switch strings.ToLower(confidence) {
	case "high":
		return toneGreen
	case "medium":
		return toneYellow
	case "low":
		return toneRed
	default:
		return toneGray
	}
}

const (
	justNowSpan = 5 * time.Second
	day         = 24 * time.Hour
	dateSpan    = 30 * day
)

func relTime(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := t.Sub(now).Abs()
	var n string
	switch {
	case d < justNowSpan:
		return "just now"
	case d < time.Minute:
		n = fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		n = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < day:
		n = fmt.Sprintf("%dh", int(d/time.Hour))
	case d < dateSpan:
		n = fmt.Sprintf("%dd", int(d/day))
	case t.UTC().Year() == now.UTC().Year():
		return t.UTC().Format("Jan 2")
	default:
		return t.UTC().Format("Jan 2, 2006")
	}
	if t.After(now) {
		return "in " + n
	}
	return n + " ago"
}

type stamp struct{ ISO, Exact, Rel string }

// utcStamp feeds the "ts" define; nil or zero renders "never".
func utcStamp(v any) *stamp {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x != nil {
			t = *x
		}
	}
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &stamp{
		ISO:   u.Format(time.RFC3339),
		Exact: u.Format("2006-01-02 15:04") + " UTC",
		Rel:   relTime(time.Now(), t),
	}
}
