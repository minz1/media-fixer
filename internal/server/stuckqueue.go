package server

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const (
	stuckUnavailable        = "Stuck queue unavailable (Sonarr and Radarr not configured)"
	stuckSettingsUnreadable = "Stored stuck-queue settings couldn't be read; saving replaces them."
	ageRounding             = time.Minute
	stuckStamp              = "2006-01-02 15:04 MST"
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
	Due           time.Time
	Targets       string
	TargetsFP     string
	Messages      []string
	Badge         string
	BadgeTone     string
	BadgeInfo     string
	ImportRefusal string
	Rejections    []string
	Buttons       []stuckButton
}

type stuckFailedView struct {
	App        string
	DownloadID string
	Title      string
	Line       string
	Op         int64
	Version    int64
}

type stuckAppView struct {
	Name                                         string
	Summary                                      string
	Counted                                      bool
	Cleared, WouldClear, Listed, Waiting, MinAge int
	Error                                        string
	Items                                        []stuckItemView
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
		notice = msgSaved
	}
	res, running := s.stuckState()
	data := s.stuckCardData(res, running, set, errs, notice, "", "")
	data["Failed"] = s.stuckFailedFor(r.Context())
	s.writeStuckCard(w, data)
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
		context.WithoutCancel(r.Context()), app, r.FormValue("download_id"), action, r.FormValue("targets_fp"),
	)
	notice, failure := restartingNotice(notice, err)
	s.renderStuckCard(r.Context(), w, notice, failure)
}

func (s *Server) stuckResume(w http.ResponseWriter, r *http.Request) {
	app, appOK := stuckqueue.AppForCode(r.FormValue("app"))
	opID, idErr := strconv.ParseInt(r.FormValue("op"), 10, 64)
	opVersion, verErr := strconv.ParseInt(r.FormValue("v"), 10, 64)
	if s.stuck == nil {
		s.renderStuckCard(r.Context(), w, "", "")
		return
	}
	if !appOK || idErr != nil || verErr != nil {
		s.renderStuckCard(r.Context(), w, "", staleNotice)
		return
	}
	notice, err := s.stuck.Resume(
		context.WithoutCancel(r.Context()), app, r.FormValue("id"), opID, opVersion,
	)
	notice, failure := restartingNotice(notice, err)
	s.renderStuckCard(r.Context(), w, notice, failure)
}

func validStuckAction(a stuckqueue.Action) bool {
	switch a {
	case stuckqueue.ActionImport, stuckqueue.ActionRegrab, stuckqueue.ActionRemove:
		return true
	}
	return false
}

func (s *Server) stuckState() (*stuckqueue.Result, bool) {
	res, ok, running := s.stuck.State()
	if !ok {
		return nil, running
	}
	return &res, running
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
	unreadable, stateErr := "", ""
	if errors.Is(err, stuckqueue.ErrCorruptSettings) {
		unreadable = stuckSettingsUnreadable
	} else if err != nil {
		stateErr = err.Error()
	}
	res, running := s.stuckState()
	card := s.stuckCardData(res, running, set, nil, notice, failure, unreadable)
	card["StateErr"] = stateErr
	card["Failed"] = s.stuckFailedFor(ctx)
	return card
}

func (s *Server) stuckCardData(
	res *stuckqueue.Result, running bool, set stuckqueue.Settings, errs map[string]string,
	notice, failure, unreadable string,
) map[string]any {
	card := map[string]any{
		tplKeyBaseURL: s.baseURL,
		"Settings":    set,
		"Errors":      errs,
		tplKeyNotice:  notice,
		tplKeyFailure: failure,
		"Unreadable":  unreadable,
		"Bounds":      stuckqueue.SettingBounds(),
		"Defaults":    stuckqueue.DefaultSettings(),
		"Running":     running,
		"StartedAt":   s.stuck.StartedAt(),
	}
	if res == nil {
		return card
	}
	card["LastRun"] = res.FinishedAt
	card["DryRun"] = res.DryRun
	card["Paused"] = res.Paused
	card["CapHit"] = res.CapHit
	apps := make([]stuckAppView, 0, len(res.Apps))
	for _, ar := range res.Apps {
		apps = append(apps, stuckApp(ar, res.MinAgeHours))
	}
	card["Apps"] = apps
	return card
}

func (s *Server) stuckFailedFor(ctx context.Context) []stuckFailedView {
	rows, err := s.db.ListStuckInState(ctx, string(stuckqueue.StateActed))
	if err != nil {
		return nil
	}
	var out []stuckFailedView
	for _, row := range rows {
		if row.ActedResult != string(stuckqueue.ResultUnfinished) {
			continue
		}
		op, opErr := s.db.Operation(ctx, row.ActedOpID)
		if opErr != nil || !operation.Resumable(op) {
			continue
		}
		code, _, _ := strings.Cut(op.Subject.ID, ":")
		out = append(out, stuckFailedView{
			App: code, DownloadID: row.DownloadID, Title: op.Title, Line: operation.Line(op),
			Op: op.ID, Version: op.Version,
		})
	}
	slices.SortFunc(out, func(a, b stuckFailedView) int { return cmp.Compare(a.Op, b.Op) })
	return out
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
	v.Counted, v.Cleared, v.WouldClear = true, ar.Cleared, ar.WouldClear
	v.Listed, v.Waiting, v.MinAge = len(ar.Items), len(ar.Waiting), minAgeHours
	for _, it := range slices.Concat(ar.Items, ar.Waiting) {
		v.Items = append(v.Items, stuckItem(it))
	}
	return v
}

func stuckItem(it stuckqueue.Item) stuckItemView {
	v := stuckItemView{
		App: string(it.App), DownloadID: it.DownloadID, Title: it.Title, Verdict: string(it.Verdict),
		Age: it.Age.Round(ageRounding).String(), Targets: it.Targets(), TargetsFP: it.TargetsFP(),
		ImportRefusal: it.ImportRefusal, Rejections: it.Rejections, Due: it.Due,
	}
	for _, r := range it.Records {
		for _, m := range r.Messages {
			if !slices.Contains(v.Messages, m) {
				v.Messages = append(v.Messages, m)
			}
		}
	}
	v.setBadge(it)
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
	set := stuckqueue.DefaultSettings()
	set.Enabled, set.DryRun = r.FormValue("enabled") == "on", r.FormValue("dry_run") == "on"
	errs := map[string]string{}
	for _, f := range []struct {
		name string
		dst  *int
	}{
		{"interval_minutes", &set.IntervalMinutes},
		{"min_age_hours", &set.MinAgeHours},
		{"import_timeout_minutes", &set.ImportTimeoutMinutes},
	} {
		n, err := strconv.Atoi(r.FormValue(f.name))
		if err != nil {
			errs[f.name] = msgWholeNumber
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

func (v *stuckItemView) setBadge(it stuckqueue.Item) {
	if it.State != stuckqueue.StateImportInflight && it.State != stuckqueue.StateActed {
		return
	}
	meta := stuckqueue.Meta(stuckqueue.Snapshot{State: it.State, Acted: it.Acted})
	if meta.Label == "" || meta.Label == "Unfinished" {
		return
	}
	v.Badge, v.BadgeTone, v.BadgeInfo = meta.Label, toneGray, "Import running"
	if meta.Tone == stuckqueue.ToneOK {
		v.BadgeTone = toneGreen
	}
	if it.Acted != nil {
		v.BadgeInfo = meta.Label + " " + it.Acted.At.UTC().Format(stuckStamp)
	}
}
