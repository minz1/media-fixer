package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/minz1/mediafixer/internal/babysit"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const errInvalidRow = "invalid row"

type proposalView struct {
	BaseURL string
	ID      int
	Show    string
	P       babysit.Proposal
	Err     string
}

func parseRows(vals []string) ([]stuckqueue.MappedFile, error) {
	rows := make([]stuckqueue.MappedFile, 0, len(vals))
	for _, v := range vals {
		ids, path, ok := strings.Cut(v, "|")
		if !ok || path == "" {
			return nil, errors.New(errInvalidRow)
		}
		row := stuckqueue.MappedFile{Path: path}
		for p := range strings.SplitSeq(ids, ",") {
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 {
				return nil, errors.New(errInvalidRow)
			}
			row.EpisodeIDs = append(row.EpisodeIDs, n)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *Server) importSeries(w http.ResponseWriter, r *http.Request) (int, client.Series, bool) {
	if !s.monitorReady(w) {
		return 0, client.Series{}, false
	}
	id, ok := s.monitorSeriesID(w, r)
	if !ok {
		return 0, client.Series{}, false
	}
	series, err := s.babysit.Sonarr.GetSeries(r.Context(), id)
	if errors.Is(err, client.ErrNotFound) {
		http.NotFound(w, r)
		return 0, client.Series{}, false
	}
	if err != nil {
		s.log.WarnContext(r.Context(), "monitoring: series read failed", "series", id, "err", err)
		http.Error(w, "Sonarr could not be read: "+err.Error(), http.StatusBadGateway)
		return 0, client.Series{}, false
	}
	return id, series, true
}

func (s *Server) monitorPropose(w http.ResponseWriter, r *http.Request) {
	id, series, ok := s.importSeries(w, r)
	if !ok {
		return
	}
	v := proposalView{BaseURL: s.baseURL, ID: id, Show: series.Title}
	p, err := s.babysit.ProposeImport(r.Context(), id, r.FormValue("download_id"))
	if err != nil {
		v.Err = err.Error()
	} else {
		v.P = p
	}
	s.renderMonitor(w, "monitor_proposal", v)
}

func (s *Server) monitorImport(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.importSeries(w, r)
	if !ok {
		return
	}
	var notice, failure string
	err := r.ParseForm()
	var rows []stuckqueue.MappedFile
	if err == nil {
		rows, err = parseRows(r.Form["row"])
	}
	switch {
	case err != nil:
		failure = err.Error()
	case s.stuck == nil:
		failure = "The stuck queue isn't running."
	default:
		notice, err = s.stuck.ImportMapped(
			context.WithoutCancel(r.Context()), id, r.FormValue("download_id"), r.FormValue("targets_fp"), rows,
		)
		notice, failure = restartingNotice(notice, err)
	}
	s.renderMonitorShow(w, r, id, tplMonitorCard, notice, failure, false)
}
