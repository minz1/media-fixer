package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/sweep"
)

func TestLayout_EveryPageHasShell(t *testing.T) {
	t.Parallel()
	e := newMonitorTestServer(t)
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "T"}
	lifecycletest.Incident(t, e.db, inc)
	pages := []struct{ path, tab string }{
		{"/media/", "/media/"},
		{"/media/incidents/" + inc.ID, "/media/"},
		{"/media/incidents/" + inc.ID + "/transcript", "/media/"},
		{"/media/selftest", "/media/selftest"},
		{"/media/maintenance", "/media/maintenance"},
		{"/media/monitoring", "/media/monitoring"},
		{"/media/monitoring/7", "/media/monitoring"},
	}
	for _, p := range pages {
		rec := do(t, e.srv, http.MethodGet, p.path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", p.path, rec.Code)
		}
		body := rec.Body.String()
		mustContain(t, body, `id="progress"`, `id="pause-toggle"`, "htmx:before:swap")
		if n := strings.Count(body, `id="notice"`); n != 1 {
			t.Errorf("GET %s: %d notice regions, want 1", p.path, n)
		}
		if n := strings.Count(body, `aria-current="page"`); n != 1 {
			t.Errorf("GET %s: %d aria-current tabs, want 1", p.path, n)
		}
		if !strings.Contains(body, `href="`+p.tab+`" aria-current="page"`) {
			t.Errorf("GET %s: tab %s is not the current one", p.path, p.tab)
		}
	}
}

func TestNotice_ActionEmitsOOB(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)
	rec := do(t, env.srv, http.MethodPost, timersPath, validTimersForm())
	mustContain(t, rec.Body.String(), `<div id="notice" hx-swap-oob="innerHTML"`, "Saved")

	bad := validTimersForm()
	bad.Set("verify_checks", "abc")
	rec = do(t, env.srv, http.MethodPost, timersPath, bad)
	body := rec.Body.String()
	if strings.Contains(body, "hx-swap-oob") {
		t.Errorf("a field error went out as an OOB notice:\n%s", body)
	}
	mustContain(t, body, msgWholeNumberText)

	s := newStuckEnv(t)
	path := "/media/maintenance/stuck-queue/settings"
	ok := url.Values{
		"enabled": {"on"}, "interval_minutes": {"30"}, "min_age_hours": {"12"}, "import_timeout_minutes": {"60"},
	}
	mustContain(t, do(t, s.srv, http.MethodPost, path, ok).Body.String(),
		`<div id="notice" hx-swap-oob="innerHTML"`, "Saved.")
	ok.Set("interval_minutes", "abc")
	body = do(t, s.srv, http.MethodPost, path, ok).Body.String()
	if strings.Contains(body, "hx-swap-oob") {
		t.Errorf("a field error went out as an OOB notice:\n%s", body)
	}
	mustContain(t, body, msgWholeNumberText)
}

const msgWholeNumberText = "must be a whole number"

func TestNotice_OneRegionOnErrorPage(t *testing.T) {
	t.Parallel()
	s := newStuckEnv(t)
	ctx := context.Background()
	if err := s.db.SetSetting(ctx, stuckqueue.SettingsKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetSetting(ctx, incident.TimersKey, "{"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for name, c := range map[string]context.Context{"unreadable": ctx, "load failure": cancelled} {
		req := httptest.NewRequestWithContext(c, http.MethodGet, "/media/maintenance", nil)
		rec := httptest.NewRecorder()
		s.srv.Handler().ServeHTTP(rec, req)
		body := rec.Body.String()
		if n := strings.Count(body, `id="notice"`); n != 1 {
			t.Errorf("%s: %d notice regions, want 1", name, n)
		}
		if strings.Contains(body, "hx-swap-oob") {
			t.Errorf("%s: a page load emitted an OOB notice", name)
		}
		if n := strings.Count(body, `class="notice notice-err"`); n < 2 {
			t.Errorf("%s: %d inline error notices, want at least 2:\n%s", name, n, body)
		}
		_, stuck, _ := strings.Cut(body, `id="stuck-queue"`)
		stuck, _, _ = strings.Cut(stuck, `id="incident-timers"`)
		if !strings.Contains(stuck, `class="notice notice-err"`) {
			t.Errorf("%s: the stuck card shows no error for its settings:\n%s", name, stuck)
		}
	}
}

func TestPause_HXRequestRendersToggle(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)
	cases := []struct{ path, button, notice string }{
		{"/media/pause", "Resume", "Autonomous actions paused."},
		{"/media/resume", "Pause agent", "Autonomous actions resumed."},
	}
	for _, c := range cases {
		rec := doWith(t, srv, http.MethodPost, c.path, url.Values{}, map[string]string{"HX-Request": "true"})
		if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
			t.Fatalf("%s: status %d, Location %q", c.path, rec.Code, rec.Header().Get("Location"))
		}
		mustContain(t, rec.Body.String(), `id="pause-toggle"`, c.button, `hx-swap-oob="innerHTML"`, c.notice)
		rec = do(t, srv, http.MethodPost, c.path, url.Values{})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/media/" {
			t.Errorf("%s without HX-Request: %d -> %q", c.path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestIncidentAction_DoneNotice(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := &db.Incident{
		Status: db.StatusAgentFixed, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Dropped",
	}
	lifecycletest.Incident(t, database, inc)
	page := "/media/incidents/" + inc.ID
	rec := doWith(t, srv, http.MethodPost, page+"/rerun", versionForm(t, database, inc.ID), nil)
	if rec.Header().Get("Location") != page+"?done=rerun" {
		t.Fatalf("Location = %q", rec.Header().Get("Location"))
	}
	for key, want := range map[string]string{
		"resolve": "Marked resolved.", "rerun": "Rerun started.", "keep": "Keep searching started.",
		"unlock": "Unlocked.", "approve": "Escalation approved; running it now.",
	} {
		body := do(t, srv, http.MethodGet, page+"?done="+key, nil).Body.String()
		re := regexp.MustCompile(`id="notice"[^>]*>\s*<p class="notice notice-ok">` + regexp.QuoteMeta(want))
		if !re.MatchString(body) {
			t.Errorf("done=%s: #notice does not hold %q", key, want)
		}
	}
	body := do(t, srv, http.MethodGet, page+"?done=zzmarker", nil).Body.String()
	if strings.Contains(body, "zzmarker") || !regexp.MustCompile(`id="notice"[^>]*>\s*</div>`).MatchString(body) {
		t.Errorf("an unknown done key leaked or filled #notice")
	}
}

func TestStuckResults_RunningShowsStarted(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	env.arr.mu.Lock()
	env.arr.hold = hold
	env.arr.mu.Unlock()
	if err := env.sch.RunNow(); err != nil {
		t.Fatal(err)
	}
	body := do(t, env.srv, http.MethodGet, "/media/maintenance/stuck-queue/results", nil).Body.String()
	mustContain(t, body, "Running…", " started ", "<time datetime=")
	release()
	env.sch.Wait()
}

func TestSweepStatus_StartingWhenZero(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)
	render := func(p sweep.Progress) string {
		var b strings.Builder
		err := srv.ExecuteTemplateForTest(&b, "sweep_status", map[string]any{
			"BaseURL": "/media", "Active": true, "Progress": p, "Next": (*time.Time)(nil), "Due": false,
		})
		if err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	body := render(sweep.Progress{})
	mustContain(t, body, "starting…")
	if strings.Contains(body, "never") || strings.Contains(body, "<time") {
		t.Errorf("a zero start rendered a time:\n%s", body)
	}
	mustContain(t, render(sweep.Progress{Phase: "season pass", Started: time.Now()}),
		"Running…", " started ", "<time datetime=")
}

var tagRe = regexp.MustCompile(`(?s)<(?:form|button)\b[^>]*>`)

func TestForms_DisableWhileRunning(t *testing.T) {
	t.Parallel()
	slowActions := []string{
		"/selftest/run", "/pack-sweep/run", "/stuck-queue/run", "/search/packs", "/search/singles", "/explain",
		"/escalation-preview", "/import/propose", "/releases",
	}
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tagRe.FindAllString(string(raw), -1) {
		if !strings.Contains(tag, "hx-post") && !strings.Contains(tag, "hx-get") {
			continue
		}
		if !strings.Contains(tag, "hx-disable=") {
			t.Errorf("request element without hx-disable: %s", tag)
		}
		for _, slow := range slowActions {
			if strings.Contains(tag, slow) && !strings.Contains(tag, `hx-indicator="this, #progress"`) {
				t.Errorf("slow action without the progress indicator: %s", tag)
			}
		}
	}
}

func scanTags(src, open string) []string {
	var tags []string
	for i := 0; ; {
		j := strings.Index(src[i:], open)
		if j < 0 {
			return tags
		}
		start := i + j
		k := start + len(open)
		var quote byte
		for k < len(src) {
			switch {
			case strings.HasPrefix(src[k:], "{{"):
				k += strings.Index(src[k:], "}}") + 2
				continue
			case quote != 0:
				if src[k] == quote {
					quote = 0
				}
			case src[k] == '"' || src[k] == '\'':
				quote = src[k]
			}
			if quote == 0 && src[k] == '>' {
				break
			}
			k++
		}
		tags = append(tags, src[start:k+1])
		i = k + 1
	}
}

var (
	classRe   = regexp.MustCompile(`class="([^"]*)"`)
	variantRe = regexp.MustCompile(`\bbtn-(?:primary|secondary|danger|link)\b`)
	btnRe     = regexp.MustCompile(`(^|\s)btn(\s|$)`)
)

func TestButtons_OneSet(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	tags := scanTags(src, "<button")
	for _, a := range scanTags(src, "<a") {
		if m := classRe.FindStringSubmatch(a); m != nil && strings.Contains(m[1], "btn") {
			tags = append(tags, a)
		}
	}
	if len(tags) == 0 {
		t.Fatal("no buttons found")
	}
	for _, tag := range tags {
		m := classRe.FindStringSubmatch(tag)
		if m == nil {
			t.Errorf("button without a class: %s", tag)
			continue
		}
		class := m[1]
		if !btnRe.MatchString(class) {
			t.Errorf("no btn class: %s", tag)
		}
		if n := len(variantRe.FindAllString(class, -1)); n != 1 {
			t.Errorf("%d btn variants, want 1: %s", n, tag)
		}
		for _, bad := range []string{"bg-", "px-", "py-", "rounded"} {
			if strings.Contains(class, bad) {
				t.Errorf("button carries %q: %s", bad, tag)
			}
		}
	}
}

func TestButtons_DangerOnlyDestructive(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, word := range []string{"Pause agent", "Forget"} {
		i := strings.Index(src, word)
		if i < 0 {
			t.Fatalf("%q not found", word)
		}
		tag := src[strings.LastIndex(src[:i], "<button"):i]
		if !strings.Contains(tag, "btn-secondary") {
			t.Errorf("%q is not btn-secondary: %s", word, tag)
		}
	}
	if !strings.Contains(src, `class="btn btn-danger">`+"\n      Approve &amp; Run") {
		t.Error("Approve & Run is no longer danger")
	}
}

func TestTables_ScrollWrapped(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for i := 0; ; {
		j := strings.Index(src[i:], "<table")
		if j < 0 {
			break
		}
		at := i + j
		before := strings.TrimRight(src[:at], " \t\n")
		if !strings.HasSuffix(before, `<div class="overflow-x-auto">`) {
			line := strings.Count(src[:at], "\n") + 1
			t.Errorf("dashboard.html:%d: <table> not directly inside <div class=\"overflow-x-auto\">", line)
		}
		i = at + 1
	}
}

var cssRuleRe = regexp.MustCompile(`(?m)^\s*(\.[\w-]+)[\s,{:]`)

func TestDarkOverrides_AfterLightRules(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	style := src[strings.Index(src, "<style>"):strings.Index(src, "</style>")]
	start := strings.Index(style, "@media (prefers-color-scheme: dark)")
	end := start + strings.Index(style[start:], "\n    }\n")
	if start < 0 || end < start {
		t.Fatal("dark media block not found")
	}
	light := style[:start] + style[end:]
	for _, m := range cssRuleRe.FindAllStringSubmatch(style[start:end], -1) {
		sel := m[1]
		if i := strings.Index(light, "\n    "+sel+" "); i >= 0 && i > start {
			t.Errorf("light %s rule sits after the dark block, so it wins in dark mode", sel)
		}
	}
}

var (
	actionRe     = regexp.MustCompile(`\{\{-?\s*(if|range|with|define|block|end)\b`)
	includeRe    = regexp.MustCompile(`\{\{-?\s*template\s+"([^"]+)"[^}]*\}\}`)
	findButtonRe = regexp.MustCompile(`hx-disable="(find button[^"]*)"`)
	buttonTagRe  = regexp.MustCompile(`<button\b[^>]*>`)
	typeAttrRe   = regexp.MustCompile(`\stype=`)
)

func inRange(text string, at int) bool {
	var open []string
	for _, m := range actionRe.FindAllStringSubmatchIndex(text[:at], -1) {
		if kind := text[m[2]:m[3]]; kind != "end" {
			open = append(open, kind)
		} else if len(open) > 0 {
			open = open[:len(open)-1]
		}
	}
	return slices.Contains(open, "range")
}

func defineBody(src, name string) string {
	open := `{{define "` + name + `"}}`
	start := strings.Index(src, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	depth := 1
	for _, m := range actionRe.FindAllStringSubmatchIndex(src[start:], -1) {
		if src[start+m[2]:start+m[3]] != "end" {
			depth++
			continue
		}
		if depth--; depth == 0 {
			return src[start : start+m[0]]
		}
	}
	return ""
}

func expandIncludes(src, text string) string {
	return includeRe.ReplaceAllStringFunc(text, func(call string) string {
		return expandIncludes(src, defineBody(src, includeRe.FindStringSubmatch(call)[1]))
	})
}

func TestFormDisable_TargetsOnlyTheSubmit(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	forms := 0
	for _, part := range strings.Split(src, "<form")[1:] {
		form, _, _ := strings.Cut(part, "</form>")
		head, _, _ := strings.Cut(form, ">")
		m := findButtonRe.FindStringSubmatch(head)
		if m == nil {
			continue
		}
		forms++
		body := expandIncludes(src, form)
		var submits []int
		buttons := buttonTagRe.FindAllStringIndex(body, -1)
		for _, b := range buttons {
			tag := body[b[0]:b[1]]
			if m[1] == "find button" || strings.Contains(tag, `type="submit"`) || !typeAttrRe.MatchString(tag) {
				submits = append(submits, b[0])
			}
		}
		switch {
		case m[1] != "find button" && m[1] != "find button[type=submit]":
			t.Errorf("unknown hx-disable selector %q:%s", m[1], head)
		case len(submits) != 1:
			t.Errorf("form with hx-disable=%q has %d matching buttons, so it may disable the wrong one:%s",
				m[1], len(submits), head)
		case inRange(body, submits[0]):
			t.Errorf("form with hx-disable=%q repeats its button in a range:%s", m[1], head)
		}
	}
	if want := strings.Count(src, `hx-disable="find button`); forms != want {
		t.Fatalf("checked %d find-button forms, template has %d", forms, want)
	}
}

func TestPause_WriteFailureIsAnError(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	rec := doWith(t, srv, http.MethodPost, "/media/pause", url.Values{}, map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "paused.") {
		t.Errorf("status %d, body %q; want 500 with no success notice", rec.Code, rec.Body.String())
	}
}
