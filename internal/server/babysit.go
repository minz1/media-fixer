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
	tplBabysitList   = "babysit_list"
	tplBabysitCard   = "babysit_show_card"
	explainConfirm   = "Explain makes one AI call over this show's history. Continue?"
	searchConfirmEnd = "Each season searched counts as an attempt and doubles its backoff."
)

type babysitListView struct {
	BaseURL     string
	Unavailable string
	List        babysit.List
	Notice      string
	Failure     string
}

type explainView struct {
	BaseURL string
	ID      int
	Text    string
	At      time.Time
	Stale   bool
	Err     string
	Can     bool
	Confirm string
}

type babysitShowView struct {
	BaseURL        string
	ID             int
	Title          string
	Show           babysit.Show
	Queue          []stuckItemView
	StuckAfter     int
	Status         sweepStatus
	ConfirmPacks   string
	ConfirmSingles string
	Explain        explainView
	Notice         string
	Failure        string
}

func (s *Server) SetBabysit(b *babysit.Service) {
	s.babysit = b
}

func parseSeriesID(raw string) (int, bool) {
	n, err := strconv.Atoi(raw)
	return n, err == nil && n > 0
}

func (s *Server) babysitReady(w http.ResponseWriter) bool {
	if s.babysit == nil || s.sweep == nil {
		http.Error(w, sweepUnavailable, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) babysitSeriesID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, ok := parseSeriesID(chi.URLParam(r, "seriesID"))
	if !ok {
		http.NotFound(w, r)
	}
	return id, ok
}

func (s *Server) renderBabysit(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, name, data)
}

func (s *Server) babysitIndex(w http.ResponseWriter, r *http.Request) {
	if s.babysit == nil || s.sweep == nil {
		s.renderBabysit(w, "babysit", babysitListView{BaseURL: s.baseURL, Unavailable: sweepUnavailable})
		return
	}
	s.renderBabysitList(w, r, "babysit", "", "")
}

func (s *Server) renderBabysitList(w http.ResponseWriter, r *http.Request, name, notice, failure string) {
	l, err := s.babysit.List(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderBabysit(w, name, babysitListView{BaseURL: s.baseURL, List: l, Notice: notice, Failure: failure})
}

func (s *Server) babysitShow(w http.ResponseWriter, r *http.Request) {
	if !s.babysitReady(w) {
		return
	}
	if id, ok := s.babysitSeriesID(w, r); ok {
		s.renderBabysitShow(w, r, id, "babysit_show", "", "", false)
	}
}

func (s *Server) renderBabysitShow(
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
	v := babysitShowView{
		BaseURL: s.baseURL, ID: id, Title: showTitle(sh, id), Show: sh, StuckAfter: set.StuckAfter,
		Status: s.sweepStatus(started), Notice: notice, Failure: failure,
		Explain: explainView{BaseURL: s.baseURL, ID: id, Confirm: explainConfirm},
	}
	for _, it := range sh.Queue {
		v.Queue = append(v.Queue, stuckItem(it))
	}
	v.ConfirmPacks, v.ConfirmSingles = s.searchConfirms(set, v.Title)
	e := &v.Explain
	e.Can = sh.Series != nil && sh.SonarrErr == "" && sh.Stuck && !sh.CacheFresh
	if sh.Cached != nil {
		e.Text, e.At, e.Stale = sh.Cached.Text, sh.Cached.CreatedAt, !sh.CacheFresh && sh.SonarrErr == ""
	}
	s.renderBabysit(w, name, v)
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

func (s *Server) babysitSearch(w http.ResponseWriter, r *http.Request) {
	if !s.babysitReady(w) {
		return
	}
	id, ok := s.babysitSeriesID(w, r)
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
		s.renderBabysitShow(w, r, id, tplBabysitCard, "", err.Error(), false)
		return
	}
	s.renderBabysitShow(w, r, id, tplBabysitCard, "Search started.", "", true)
}

func (s *Server) babysitSetExcluded(excluded bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.babysitReady(w) {
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
			s.renderBabysitShow(w, r, id, tplBabysitCard, notice, failure, false)
			return
		}
		s.renderBabysitList(w, r, tplBabysitList, notice, failure)
	}
}

func (s *Server) babysitExplain(w http.ResponseWriter, r *http.Request) {
	if !s.babysitReady(w) {
		return
	}
	id, ok := s.babysitSeriesID(w, r)
	if !ok {
		return
	}
	ex, err := s.babysit.Explain(r.Context(), id)
	if errors.Is(err, client.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	v := explainView{BaseURL: s.baseURL, ID: id, Confirm: explainConfirm}
	if err != nil {
		v.Err, v.Can = err.Error(), !errors.Is(err, babysit.ErrNotStuck)
	} else {
		v.Text, v.At = ex.Text, ex.CreatedAt
	}
	s.renderBabysit(w, "babysit_explain", v)
}
