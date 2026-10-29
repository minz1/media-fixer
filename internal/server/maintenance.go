package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/minz1/mediafixer/internal/sweep"
)

const (
	sweepRunsShown          = 10
	sweepUnavailable        = "sweep unavailable (Sonarr not configured)"
	sweepSettingsUnreadable = "Stored sweep settings couldn't be read; saving replaces them."
	triggerManual           = "manual"
	lookupRounding          = 100 * time.Millisecond
)

type sweepSeries struct {
	ID       int
	Title    string
	Excluded bool
}

type sweepStatus struct {
	BaseURL  string
	Active   bool
	Progress sweep.Progress
	Next     *time.Time
	Due      bool
}

func (s *Server) SetSweep(sch *sweep.Scheduler) {
	s.sweep = sch
}

func (s *Server) maintenanceIndex(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{tplKeyBaseURL: s.baseURL}
	if s.sweep != nil {
		set, unreadable, err := s.loadSweepSettings(r.Context())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data["Card"] = s.sweepCard(r.Context(), set, nil, "", unreadable, false)
	}
	data["Stuck"] = s.stuckCardFor(r.Context(), "", "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "maintenance", data)
}

func (s *Server) sweepSettingsSave(w http.ResponseWriter, r *http.Request) {
	if !s.sweepConfigured(w) {
		return
	}
	set, errs := parseSweepSettings(r)
	notice := ""
	if len(errs) == 0 {
		if err := sweep.SaveSettings(r.Context(), s.db, set, time.Now()); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !set.Enabled {
			s.sweep.CancelLive(sweep.ErrDisabled)
		}
		notice = "Saved."
	}
	s.renderSweepCard(r.Context(), w, set, errs, notice, "", false)
}

func (s *Server) sweepRun(w http.ResponseWriter, r *http.Request) {
	if !s.sweepConfigured(w) {
		return
	}
	dry := r.FormValue("dry") == "1"
	notice, failure := "Run started.", ""
	if dry {
		notice = "Dry run started."
	}
	if err := s.sweep.RunNow(triggerManual, dry); err != nil {
		notice, failure = "", err.Error()
	}
	set, unreadable, err := s.loadSweepSettings(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if unreadable != "" {
		notice, failure = "", unreadable
	}
	s.renderSweepCard(r.Context(), w, set, nil, notice, failure, failure == "")
}

func (s *Server) sweepStatusFragment(w http.ResponseWriter, _ *http.Request) {
	if !s.sweepConfigured(w) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "sweep_status", s.sweepStatus(false))
}

func (s *Server) sweepForget(w http.ResponseWriter, r *http.Request) {
	if !s.sweepConfigured(w) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err = s.db.DeleteSweepRefused(r.Context(), id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	set, unreadable, err := s.loadSweepSettings(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderSweepCard(r.Context(), w, set, nil, "", unreadable, false)
}

func (s *Server) loadSweepSettings(ctx context.Context) (sweep.Settings, string, error) {
	set, err := sweep.LoadSettings(ctx, s.db)
	if errors.Is(err, sweep.ErrCorruptSettings) {
		return set, sweepSettingsUnreadable, nil
	}
	return set, "", err
}

func (s *Server) sweepConfigured(w http.ResponseWriter) bool {
	if s.sweep == nil {
		http.Error(w, sweepUnavailable, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) renderSweepCard(
	ctx context.Context, w http.ResponseWriter, set sweep.Settings, errs map[string]string,
	notice, failure string, started bool,
) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "sweep_card", s.sweepCard(ctx, set, errs, notice, failure, started))
}

func (s *Server) sweepCard(
	ctx context.Context, set sweep.Settings, errs map[string]string, notice, failure string, started bool,
) map[string]any {
	card := map[string]any{
		tplKeyBaseURL: s.baseURL,
		"Settings":    set,
		"Errors":      errs,
		"Notice":      notice,
		"Failure":     failure,
		"Status":      s.sweepStatus(started),
		"Weekdays":    weekdayNames(),
	}
	sonarr := s.sweep.Runner().Sonarr
	if series, err := sonarr.ListSeries(ctx); err != nil {
		card["SeriesError"] = err.Error()
	} else {
		list := make([]sweepSeries, 0, len(series))
		for _, x := range series {
			list = append(list, sweepSeries{x.ID, x.Title, slices.Contains(set.ExcludedSeries, x.ID)})
		}
		slices.SortFunc(list, func(a, b sweepSeries) int {
			return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
		})
		card["Series"] = list
	}
	if flags, err := sonarr.DownloadClientConfig(ctx); err != nil {
		card["FlagsError"] = err.Error()
	} else {
		card["Flags"] = flags
	}
	if runs, err := s.db.ListSweepRuns(ctx, sweepRunsShown); err != nil {
		card["RunsError"] = err.Error()
	} else {
		card["Runs"] = runs
	}
	now := time.Now()
	if refused, err := s.db.ListSweepRefused(ctx, now.Add(-sweep.BlockedMemory)); err != nil {
		card["RefusedError"] = err.Error()
	} else {
		card["Refused"] = sweep.Blocking(refused, now)
	}
	return card
}

func (s *Server) sweepStatus(started bool) sweepStatus {
	st := sweepStatus{BaseURL: s.baseURL}
	st.Progress, st.Active = s.sweep.Runner().Progress()
	st.Active = st.Active || started
	now := time.Now()
	if st.Next = s.sweep.NextRun(now); st.Next != nil {
		st.Due = !st.Next.After(now)
	}
	return st
}

func parseSweepSettings(r *http.Request) (sweep.Settings, map[string]string) {
	set := sweep.Settings{
		Enabled:        r.FormValue("enabled") == "on",
		Anime:          r.FormValue("anime") == "on",
		ExcludedSeries: []int{},
	}
	errs := map[string]string{}
	for _, f := range []struct {
		name string
		dst  *int
	}{
		{"weekday", &set.Weekday},
		{"hour_utc", &set.HourUTC},
		{"min_missing", &set.MinMissing},
		{"max_grabs", &set.MaxGrabs},
		{"max_lookups", &set.MaxLookups},
		{"max_searches", &set.MaxSearches},
	} {
		n, err := strconv.Atoi(r.FormValue(f.name))
		if err != nil {
			errs[f.name] = "must be a whole number"
			continue
		}
		*f.dst = n
	}
	for _, v := range r.PostForm["excluded_series"] {
		if id, err := strconv.Atoi(v); err == nil {
			set.ExcludedSeries = append(set.ExcludedSeries, id)
		}
	}
	for k, v := range set.Validate() {
		if _, seen := errs[k]; !seen {
			errs[k] = v
		}
	}
	return set, errs
}

func weekdayNames() []string {
	var names []string
	for d := time.Sunday; d <= time.Saturday; d++ {
		names = append(names, d.String())
	}
	return names
}

func sweepColor(status string) string {
	switch status {
	case "done":
		return badgeGreen
	case "running":
		return "bg-blue-100 text-blue-800"
	case "failed", "refused":
		return badgeRed
	case "paused", "interrupted":
		return badgeYellow
	default:
		return badgeGray
	}
}

func seasonLabel(n *int) string {
	if n == nil {
		return ""
	}
	return fmt.Sprintf("S%02d", *n)
}

func lookupDuration(ms *int) string {
	if ms == nil {
		return ""
	}
	return (time.Duration(*ms) * time.Millisecond).Round(lookupRounding).String()
}
