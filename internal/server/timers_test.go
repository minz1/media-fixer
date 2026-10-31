package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

const timersPath = "/media/maintenance/incident-timers/settings"

var timersFormRe = regexp.MustCompile(`(?s)<form[^>]*` + regexp.QuoteMeta(timersPath) + `.*?</form>`)

func validTimersForm() url.Values {
	form := url.Values{}
	var t incident.Timers
	for _, f := range t.Fields() {
		form.Set(f.Name, strconv.Itoa(f.Default))
	}
	return form
}

func TestTimersCard_FormRoundTrip(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)
	ctx := context.Background()

	body := do(t, env.srv, http.MethodGet, "/media/maintenance", nil).Body.String()
	mustContain(t, body, `id="incident-timers"`)
	form := timersFormRe.FindString(body)
	if form == "" {
		t.Fatalf("incident timers form not found:\n%s", body)
	}
	post := browserForm(form)

	var tm incident.Timers
	fields := tm.Fields()
	if len(post) != len(fields) {
		t.Errorf("form has %d inputs, want %d: %v", len(post), len(fields), post)
	}
	for _, f := range fields {
		if _, ok := post[f.Name]; !ok {
			t.Errorf("form does not render %q", f.Name)
		}
		mustContain(t, form, `name="`+f.Name+`" value="`+strconv.Itoa(f.Default)+`" min="`+strconv.Itoa(f.Lo)+
			`" max="`+strconv.Itoa(f.Hi)+`"`)
	}
	last := -1
	for _, g := range []string{"Agent runs", "Verification", "Reporter notes", "Download tracking", "Explain", "Stuck reads"} {
		i := strings.Index(form, g)
		if i <= last {
			t.Errorf("group %q missing or out of order", g)
		}
		last = i
	}

	want := incident.DefaultTimers()
	for _, f := range fields {
		post.Set(f.Name, strconv.Itoa(f.Hi))
		want.Set(f.Name, f.Hi)
	}
	rec := do(t, env.srv, http.MethodPost, timersPath, post)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Saved.")
	got, err := incident.LoadTimers(ctx, env.db)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("saved %+v, want %+v", got, want)
	}
}

func TestTimersCard_FieldErrors(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)

	form := validTimersForm()
	form.Set("verify_checks", "21")
	rec := do(t, env.srv, http.MethodPost, timersPath, form)
	mustContain(t, rec.Body.String(), "must be between 1 and 20")
	if _, err := env.db.GetSetting(context.Background(), incident.TimersKey); err == nil {
		t.Error("timers saved despite a field error")
	}

	form = validTimersForm()
	form.Set("stale_run_minutes", "10")
	form.Set("retry_first_minutes", "6")
	form.Set("retry_second_minutes", "6")
	rec = do(t, env.srv, http.MethodPost, timersPath, form)
	mustContain(t, rec.Body.String(), "must be at least 17")
	if strings.Contains(rec.Body.String(), "Saved.") {
		t.Error("cross-field failure reported as saved")
	}
}

func TestTimersCard_ParseErrorSkipsCrossField(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)

	form := validTimersForm()
	form.Set("retry_first_minutes", "abc")
	form.Set("retry_second_minutes", "6")
	form.Set("stale_run_minutes", "10")
	body := do(t, env.srv, http.MethodPost, timersPath, form).Body.String()
	mustContain(t, body, "must be a whole number")
	if strings.Contains(body, "must be at least") {
		t.Errorf("cross-field error rendered after a parse error:\n%s", body)
	}
}

func TestTimersSave_RequiresSameOrigin(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)
	form := validTimersForm()
	form.Set("verify_checks", "7")
	req := httptest.NewRequest(http.MethodPost, timersPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if _, err := env.db.GetSetting(context.Background(), incident.TimersKey); err == nil {
		t.Error("timers saved by a cross-origin POST")
	}
}

func TestTimersCard_CorruptRowNotice(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)
	ctx := context.Background()
	if err := env.db.SetSetting(ctx, incident.TimersKey, "{"); err != nil {
		t.Fatal(err)
	}
	if _, err := incident.LoadTimers(ctx, env.db); !errors.Is(err, incident.ErrCorruptTimers) {
		t.Fatalf("LoadTimers err = %v, want ErrCorruptTimers", err)
	}

	body := do(t, env.srv, http.MethodGet, "/media/maintenance", nil).Body.String()
	mustContain(t, body, "Stored incident timers couldn", "t be read; saving replaces them.",
		`name="verify_checks" value="5"`)
}

func TestIncidentPage_KeepSearchingDays(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	ctx := context.Background()
	tm := incident.DefaultTimers()
	tm.KeepSearchingDays = 3
	if err := incident.SaveTimers(ctx, database, tm); err != nil {
		t.Fatal(err)
	}
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "Rick and Morty"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{MediaType: "tv", Title: "Rick and Morty", Season: 9, Episode: 9, StartedAt: time.Now()}
	if err := database.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded); err != nil {
		t.Fatal(err)
	}
	mustContain(t, mustExecuteIncidentTemplate(t, srv, inc.ID), "re-checks periodically for up to 3 days")
}

func TestTimersCard_ReadErrorHidesSave(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var b strings.Builder
	if err := srv.ExecuteTemplateForTest(&b, "timers_card", srv.TimersCardForTest(ctx)); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	mustContain(t, body, "Saved incident timers couldn", "context canceled")
	if strings.Contains(body, "Save</button>") {
		t.Errorf("Save offered while the saved timers are unreadable:\n%s", body)
	}
}
