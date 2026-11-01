package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/minz1/mediafixer/internal/babysit"
)

const (
	errInvalidSeason  = "invalid season"
	errInvalidRelease = "invalid release"
)

type releasesView struct {
	BaseURL string
	ID      int
	Show    string
	F       babysit.Found
	Err     string
}

const (
	mib = 1 << 20
	gib = 1 << 30
)

func humanSize(n int64) string {
	if n >= gib {
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MiB", n/mib)
}

func formSeason(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.FormValue("season"))
	if err != nil || n < 0 {
		return 0, errors.New(errInvalidSeason)
	}
	return n, nil
}

func (s *Server) monitorReleases(w http.ResponseWriter, r *http.Request) {
	id, series, ok := s.importSeries(w, r)
	if !ok {
		return
	}
	v := releasesView{BaseURL: s.baseURL, ID: id, Show: series.Title}
	season, err := formSeason(r)
	if err == nil {
		v.F, err = s.babysit.FindReleases(r.Context(), id, season)
	}
	if err != nil {
		v.Err = err.Error()
	}
	s.renderMonitor(w, "monitor_releases", v)
}

func (s *Server) monitorGrab(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.importSeries(w, r)
	if !ok {
		return
	}
	notice, failure := "", ""
	season, err := formSeason(r)
	indexer, idErr := strconv.Atoi(r.FormValue("indexer_id"))
	if err == nil && (idErr != nil || indexer <= 0 || r.FormValue("guid") == "") {
		err = errors.New(errInvalidRelease)
	}
	if err == nil {
		notice, err = s.babysit.Grab(context.WithoutCancel(r.Context()), id, babysit.GrabRequest{
			Season: season, GUID: r.FormValue("guid"), Title: r.FormValue("title"), IndexerID: indexer,
			Override: r.FormValue("override") == "1",
			Quality:  rawForm(r, "quality"), Languages: rawForm(r, "languages"),
		})
	}
	if err != nil {
		notice, failure = "", err.Error()
	}
	s.renderMonitorShow(w, r, id, tplMonitorCard, notice, failure, false)
}

func rawForm(r *http.Request, k string) []byte {
	if v := r.FormValue(k); v != "" {
		return []byte(v)
	}
	return nil
}
