package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/minz1/mediafixer/internal/incident"
)

const (
	timersUnreadable = "Stored incident timers couldn't be read; saving replaces them."
	timersReadFailed = "Saved incident timers couldn't be read (%v); showing defaults, Save is hidden until they load."
)

type timersGroup struct {
	Name   string
	Fields []map[string]any
}

func (s *Server) timersSettingsSave(w http.ResponseWriter, r *http.Request) {
	t, errs := parseTimers(r)
	notice := ""
	if len(errs) == 0 {
		if err := incident.SaveTimers(r.Context(), s.db, t); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		notice = msgSaved
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "timers_card", s.timersCard(t, errs, notice, "", ""))
}

func (s *Server) timersCardFor(ctx context.Context) map[string]any {
	t, err := incident.LoadTimers(ctx, s.db)
	unreadable, failure := "", ""
	if errors.Is(err, incident.ErrCorruptTimers) {
		unreadable = timersUnreadable
	} else if err != nil {
		failure = fmt.Sprintf(timersReadFailed, err)
	}
	return s.timersCard(t, nil, "", unreadable, failure)
}

func (s *Server) timersCard(
	t incident.Timers, errs map[string]string, notice, unreadable, failure string,
) map[string]any {
	var groups []timersGroup
	for _, f := range t.Fields() {
		if len(groups) == 0 || groups[len(groups)-1].Name != f.Group {
			groups = append(groups, timersGroup{Name: f.Group})
		}
		g := &groups[len(groups)-1]
		g.Fields = append(g.Fields, map[string]any{
			"Label": f.Label, "Name": f.Name, "Value": f.Value, "Min": f.Lo, "Max": f.Hi,
			"Err": errs[f.Name], "Note": fmt.Sprintf("%s, default %d", f.Unit, f.Default),
		})
	}
	return map[string]any{
		tplKeyBaseURL: s.baseURL,
		"Groups":      groups,
		tplKeyNotice:  notice,
		"Unreadable":  unreadable,
		tplKeyFailure: failure,
	}
}

func parseTimers(r *http.Request) (incident.Timers, map[string]string) {
	var t incident.Timers
	errs := map[string]string{}
	for _, f := range t.Fields() {
		n, err := strconv.Atoi(r.FormValue(f.Name))
		if err != nil {
			errs[f.Name] = msgWholeNumber
			n = 0
		}
		t.Set(f.Name, n)
	}
	for k, v := range t.Validate() {
		if _, seen := errs[k]; !seen {
			errs[k] = v
		}
	}
	return t, errs
}
