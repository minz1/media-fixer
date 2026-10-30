package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const (
	stuckUnavailable        = "Stuck queue unavailable (Sonarr and Radarr not configured)"
	stuckSettingsUnreadable = "Stored stuck-queue settings couldn't be read; saving replaces them."
	stuckTimeLayout         = "2006-01-02 15:04 MST"
	ageRounding             = time.Minute
)

type stuckButton struct {
	Action  string
	Label   string
	Confirm string
}

type stuckItemView struct {
	App           string
	DownloadID    string
	Title         string
	Verdict       string
	Age           string
	Due           string
	Targets       string
	Messages      []string
	ImportRefusal string
	Rejections    []string
	Buttons       []stuckButton
}

type stuckAppView struct {
	Name    string
	Summary string
	Error   string
	Items   []stuckItemView
}

func (s *Server) SetStuckQueue(sch *stuckqueue.Scheduler) {
	s.stuck = sch
}

func (s *Server) stuckResults(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "stuck_results", s.stuckCardFor(r.Context(), "", ""))
}

func (s *Server) stuckSettingsSave(w http.ResponseWriter, r *http.Request) {
	if s.stuck == nil {
		s.renderStuckCard(r.Context(), w, "", "")
		return
	}
	set, errs := parseStuckSettings(r)
	notice := ""
	if len(errs) == 0 {
		if err := stuckqueue.SaveSettings(r.Context(), s.db, set); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		notice = "Saved."
	}
	s.writeStuckCard(w, s.stuckCardData(s.lastStuck(), set, errs, notice, "", ""))
}

func (s *Server) stuckRun(w http.ResponseWriter, r *http.Request) {
	if s.stuck == nil {
		s.renderStuckCard(r.Context(), w, "", "")
		return
	}
	notice, failure := "Run started.", ""
	if err := s.stuck.RunNow(); err != nil {
		notice, failure = "", err.Error()
	}
	s.renderStuckCard(r.Context(), w, notice, failure)
}

func (s *Server) stuckAction(w http.ResponseWriter, r *http.Request) {
	action := stuckqueue.Action(chi.URLParam(r, "action"))
	app := stuckqueue.App(r.FormValue("app"))
	if !validStuckAction(action) || (app != stuckqueue.AppSonarr && app != stuckqueue.AppRadarr) {
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}
	if s.stuck == nil {
		s.renderStuckCard(r.Context(), w, "", "")
		return
	}
	notice, err := s.stuck.Act(
		context.WithoutCancel(r.Context()), app, r.FormValue("download_id"), action, r.FormValue("targets"),
	)
	failure := ""
	if err != nil {
		failure = err.Error()
	}
	s.renderStuckCard(r.Context(), w, notice, failure)
}

func validStuckAction(a stuckqueue.Action) bool {
	switch a {
	case stuckqueue.ActionImport, stuckqueue.ActionRegrab, stuckqueue.ActionRemove:
		return true
	}
	return false
}

func (s *Server) lastStuck() *stuckqueue.Result {
	if s.stuck == nil {
		return nil
	}
	if res, ok := s.stuck.Last(); ok {
		return &res
	}
	return nil
}

func (s *Server) renderStuckCard(ctx context.Context, w http.ResponseWriter, notice, failure string) {
	s.writeStuckCard(w, s.stuckCardFor(ctx, notice, failure))
}

func (s *Server) writeStuckCard(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "stuck_card", data)
}

func (s *Server) stuckCardFor(ctx context.Context, notice, failure string) map[string]any {
	if s.stuck == nil {
		return map[string]any{tplKeyBaseURL: s.baseURL, "Unavailable": stuckUnavailable}
	}
	set, err := stuckqueue.LoadSettings(ctx, s.db)
	unreadable := ""
	if errors.Is(err, stuckqueue.ErrCorruptSettings) {
		unreadable = stuckSettingsUnreadable
	} else if err != nil {
		failure = err.Error()
	}
	return s.stuckCardData(s.lastStuck(), set, nil, notice, failure, unreadable)
}

func (s *Server) stuckCardData(
	res *stuckqueue.Result, set stuckqueue.Settings, errs map[string]string, notice, failure, unreadable string,
) map[string]any {
	card := map[string]any{
		tplKeyBaseURL: s.baseURL,
		"Settings":    set,
		"Errors":      errs,
		"Notice":      notice,
		"Failure":     failure,
		"Unreadable":  unreadable,
		"Bounds":      stuckqueue.SettingBounds(),
		"Running":     s.stuck.Running(),
	}
	if res == nil {
		return card
	}
	card["LastRun"] = res.FinishedAt.UTC().Format(stuckTimeLayout)
	card["DryRun"] = res.DryRun
	card["CapHit"] = res.CapHit
	apps := make([]stuckAppView, 0, len(res.Apps))
	for _, ar := range res.Apps {
		apps = append(apps, stuckApp(ar, res.MinAgeHours))
	}
	card["Apps"] = apps
	return card
}

func stuckApp(ar stuckqueue.AppResult, minAgeHours int) stuckAppView {
	v := stuckAppView{Name: ar.App.Name()}
	switch {
	case !ar.Configured:
		v.Summary = "not configured"
		return v
	case ar.Unreachable:
		v.Error = "unreachable: " + ar.Err
		return v
	}
	v.Summary = fmt.Sprintf("cleared %d, would clear %d, %d listed, %d waiting out the %d h delay",
		ar.Cleared, ar.WouldClear, len(ar.Items), len(ar.Waiting), minAgeHours)
	for _, it := range slices.Concat(ar.Items, ar.Waiting) {
		v.Items = append(v.Items, stuckItem(it))
	}
	return v
}

func stuckItem(it stuckqueue.Item) stuckItemView {
	v := stuckItemView{
		App: string(it.App), DownloadID: it.DownloadID, Title: it.Title, Verdict: string(it.Verdict),
		Age: it.Age.Round(ageRounding).String(), Targets: it.Targets(), ImportRefusal: it.ImportRefusal,
		Rejections: it.Rejections,
	}
	if !it.Due.IsZero() {
		v.Due = it.Due.UTC().Format(stuckTimeLayout)
	}
	for _, r := range it.Records {
		for _, m := range r.Messages {
			if !slices.Contains(v.Messages, m) {
				v.Messages = append(v.Messages, m)
			}
		}
	}
	for _, a := range it.Actions {
		v.Buttons = append(v.Buttons, stuckButtonFor(a, v.Targets))
	}
	return v
}

func stuckButtonFor(a stuckqueue.Action, targets string) stuckButton {
	switch a {
	case stuckqueue.ActionImport:
		return stuckButton{
			string(a),
			"Import → " + targets,
			"Import " + targets + " into the library as Sonarr mapped it?",
		}
	case stuckqueue.ActionRegrab:
		return stuckButton{string(a), "Re-grab", "Blocklist this release and search for another?"}
	case stuckqueue.ActionRemove:
		return stuckButton{
			string(a),
			"Remove",
			"Remove this download from the queue? Files and decypharr are untouched.",
		}
	}
	return stuckButton{Action: string(a), Label: string(a)}
}

func parseStuckSettings(r *http.Request) (stuckqueue.Settings, map[string]string) {
	set := stuckqueue.Settings{Enabled: r.FormValue("enabled") == "on", DryRun: r.FormValue("dry_run") == "on"}
	errs := map[string]string{}
	for _, f := range []struct {
		name string
		dst  *int
	}{
		{"interval_minutes", &set.IntervalMinutes},
		{"min_age_hours", &set.MinAgeHours},
	} {
		n, err := strconv.Atoi(r.FormValue(f.name))
		if err != nil {
			errs[f.name] = "must be a whole number"
			continue
		}
		*f.dst = n
	}
	for k, v := range set.Validate() {
		if _, seen := errs[k]; !seen {
			errs[k] = v
		}
	}
	return set, errs
}
