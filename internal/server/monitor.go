package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/minz1/mediafixer/internal/babysit"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/sweep"
)

const (
	viewShow         = "show"
	tplMonitorList   = "monitor_list"
	tplMonitorCard   = "monitor_show_card"
	searchConfirmEnd = "Each season searched counts as an attempt and doubles its backoff."
)

type monitorListView struct {
	BaseURL     string
	Unavailable string
	List        babysit.List
	Notice      string
	Failure     string
	Paused      bool
}

type explainView struct {
	BaseURL string
	ID      int
	Text    string
	At      time.Time
	Stale   bool
	Err     string
	Can     bool
}

type liveStuckView struct {
	DownloadID string
	Title      string
	Messages   []string
	Verdict    string
	Age        string
}

type monitorShowView struct {
	BaseURL        string
	ID             int
	Title          string
	Show           babysit.Show
	Live           []liveStuckView
	StuckAfter     int
	Status         sweepStatus
	ConfirmPacks   string
	ConfirmSingles string
	Explain        explainView
	Notice         string
	Failure        string
	Paused         bool
}

func (s *Server) SetBabysit(b *babysit.Service) {
	s.babysit = b
}

func parseSeriesID(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	return n, err == nil && n > 0
}

func (s *Server) monitorReady(w http.ResponseWriter) bool {
	if s.babysit == nil || s.sweep == nil {
		http.Error(w, sweepUnavailable, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) monitorSeriesID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, ok := parseSeriesID(chi.URLParam(r, "seriesID"))
	if !ok {
		http.NotFound(w, r)
	}
	return id, ok
}

func (s *Server) renderMonitor(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, name, data)
}

func (s *Server) monitorIndex(w http.ResponseWriter, r *http.Request) {
	if s.babysit == nil || s.sweep == nil {
		s.renderMonitor(w, "monitor", monitorListView{
			BaseURL: s.baseURL, Unavailable: sweepUnavailable, Paused: s.isPaused(r.Context()),
		})
		return
	}
	s.renderMonitorList(w, r, "monitor", "", "")
}

func (s *Server) renderMonitorList(w http.ResponseWriter, r *http.Request, name, notice, failure string) {
	l, err := s.babysit.List(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderMonitor(w, name, monitorListView{BaseURL: s.baseURL, List: l, Notice: notice, Failure: failure,
		Paused: s.isPaused(r.Context())})
}

func (s *Server) monitorShow(w http.ResponseWriter, r *http.Request) {
	if !s.monitorReady(w) {
		return
	}
	if id, ok := s.monitorSeriesID(w, r); ok {
		s.renderMonitorShow(w, r, id, "monitor_show", "", "", false)
	}
}

func (s *Server) renderMonitorShow(
	w http.ResponseWriter, r *http.Request, id int, name, notice, failure string, started bool,
) {
	ctx := r.Context()
	sh, err := s.babysit.Show(ctx, id)
	if errors.Is(err, client.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	set, _ := sweep.LoadSettings(ctx, s.db)
	v := monitorShowView{
		BaseURL: s.baseURL, ID: id, Title: showTitle(sh, id), Show: sh, StuckAfter: set.StuckAfter,
		Status: s.sweepStatus(ctx, started), Notice: notice, Failure: failure, Paused: s.isPaused(ctx),
		Explain: explainView{BaseURL: s.baseURL, ID: id},
	}
	for _, l := range sh.Live {
		v.Live = append(v.Live, liveStuck(l))
	}
	v.ConfirmPacks, v.ConfirmSingles = s.searchConfirms(set, v.Title)
	e := &v.Explain
	e.Can = sh.Series != nil && sh.SonarrErr == "" && sh.Stuck && !sh.CacheFresh
	if sh.Cached != nil {
		e.Text, e.At, e.Stale = sh.Cached.Text, sh.Cached.CreatedAt, !sh.CacheFresh && sh.SonarrErr == ""
	}
	s.renderMonitor(w, name, v)
}

func liveStuck(l babysit.LiveStuck) liveStuckView {
	v := liveStuckView{DownloadID: l.Download.DownloadID, Title: l.Download.Title}
	for _, r := range l.Download.Records {
		for _, m := range r.Messages {
			if m = client.RedactCredentials(m); !slices.Contains(v.Messages, m) {
				v.Messages = append(v.Messages, m)
			}
		}
	}
	if l.Last != nil {
		v.Verdict, v.Age = string(l.Last.Verdict), l.Last.Age.Round(ageRounding).String()
	}
	return v
}

func showTitle(sh babysit.Show, id int) string {
	switch {
	case sh.Series != nil:
		return sh.Series.Title
	case len(sh.Targets) > 0:
		return sh.Targets[0].SeriesTitle
	case len(sh.Items) > 0:
		return sh.Items[0].SeriesTitle
	}
	return fmt.Sprintf("Series %d", id)
}

func (s *Server) searchConfirms(set sweep.Settings, title string) (string, string) {
	packsLeft, singlesLeft := "unknown", "unknown"
	if usage, err := s.sweep.Used(time.Now()); err == nil {
		r := sweep.Remaining(set, usage)
		packsLeft = fmt.Sprintf("%d lookups and %d grabs", r.MaxLookups, r.MaxGrabs)
		singlesLeft = fmt.Sprintf("%d episode searches", r.MaxSearches)
	}
	const format = "Search %s of %s now? Budget left today: %s. %s"
	return fmt.Sprintf(format, "season packs", title, packsLeft, searchConfirmEnd),
		fmt.Sprintf(format, "missing episodes", title, singlesLeft, searchConfirmEnd)
}

func (s *Server) monitorSearch(w http.ResponseWriter, r *http.Request) {
	if !s.monitorReady(w) {
		return
	}
	id, ok := s.monitorSeriesID(w, r)
	if !ok {
		return
	}
	pass := sweep.Pass(chi.URLParam(r, "pass"))
	if !slices.Contains([]sweep.Pass{sweep.PassPacks, sweep.PassSingles}, pass) {
		http.NotFound(w, r)
		return
	}
	series, err := s.babysit.Sonarr.GetSeries(r.Context(), id)
	if errors.Is(err, client.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err == nil {
		err = s.sweep.RunSeries(series, pass)
	}
	if err != nil {
		s.renderMonitorShow(w, r, id, tplMonitorCard, "", err.Error(), false)
		return
	}
	s.renderMonitorShow(w, r, id, tplMonitorCard, "Search started.", "", true)
}

func (s *Server) monitorSetExcluded(excluded bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.monitorReady(w) {
			return
		}
		id, ok := parseSeriesID(r.FormValue("series_id"))
		if !ok {
			http.Error(w, "invalid series_id", http.StatusBadRequest)
			return
		}
		notice, failure := msgSaved, ""
		if err := sweep.SetExcluded(r.Context(), s.db, id, excluded); errors.Is(err, sweep.ErrCorruptSettings) {
			notice, failure = "", err.Error()
		} else if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if r.FormValue("view") == viewShow {
			s.renderMonitorShow(w, r, id, tplMonitorCard, notice, failure, false)
			return
		}
		s.renderMonitorList(w, r, tplMonitorList, notice, failure)
	}
}

func (s *Server) monitorExplain(w http.ResponseWriter, r *http.Request) {
	if !s.monitorReady(w) {
		return
	}
	id, ok := s.monitorSeriesID(w, r)
	if !ok {
		return
	}
	ex, err := s.babysit.Explain(r.Context(), id)
	if errors.Is(err, client.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	v := explainView{BaseURL: s.baseURL, ID: id}
	if err != nil {
		v.Err, v.Can = err.Error(), !errors.Is(err, babysit.ErrNotStuck)
		if c, getErr := s.db.GetBabysitExplanation(r.Context(), id); getErr == nil {
			v.Text, v.At, v.Stale = c.Text, c.CreatedAt, true
		}
	} else {
		v.Text, v.At = ex.Text, ex.CreatedAt
	}
	s.renderMonitor(w, "monitor_explain", v)
}
