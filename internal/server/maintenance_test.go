package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/server"
	"github.com/minz1/mediafixer/internal/sweep"
)

type maintSonarr struct {
	hold    chan struct{}
	lookups chan struct{}
}

func (f *maintSonarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	aired := time.Now().Add(-48 * time.Hour)
	var body any
	switch r.URL.Path {
	case "/api/v3/config/downloadclient":
		body = client.DownloadClientConfig{AutoRedownloadFailed: true}
	case "/api/v3/queue/details":
		body = []any{}
	case "/api/v3/series":
		body = []client.Series{
			{ID: 1, Title: "Severance", Monitored: true, SeriesType: "standard"},
			{ID: 2, Title: "Andor", Monitored: true, SeriesType: "standard"},
		}
	case "/api/v3/episode":
		body = []client.Episode{
			{ID: 10, SeriesID: 1, SeasonNumber: 1, EpisodeNumber: 1, Monitored: true, AirDateUtc: &aired},
		}
	case "/api/v3/release":
		f.lookups <- struct{}{}
		select {
		case <-f.hold:
		case <-r.Context().Done():
			return
		}
		body = []any{}
	default:
		body = []any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

type maintEnv struct {
	srv    *server.Server
	db     *db.DB
	sch    *sweep.Scheduler
	sonarr *maintSonarr
	cancel context.CancelFunc
}

func newMaintenanceTestServer(t *testing.T) *maintEnv {
	t.Helper()
	srv, database := newDashboardTestServer(t)
	fake := &maintSonarr{hold: make(chan struct{}), lookups: make(chan struct{}, 8)}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	r := &sweep.Runner{
		Sonarr: client.NewArr(ts.URL, "k"), DB: database, Now: time.Now,
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
	sch := sweep.NewScheduler(ctx, r, database)
	srv.SetSweep(sch)
	env := &maintEnv{srv: srv, db: database, sch: sch, sonarr: fake, cancel: cancel}
	t.Cleanup(env.stop)
	return env
}

func (e *maintEnv) stop() {
	e.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runs, _ := e.db.ListSweepRuns(context.Background(), 1)
		if _, active := e.sch.Runner().Progress(); !active && (len(runs) == 0 || runs[0].Status != "running") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *maintEnv) startHeldRun(t *testing.T) {
	t.Helper()
	if err := e.sch.RunNow("manual", true); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	select {
	case <-e.sonarr.lookups:
	case <-time.After(5 * time.Second):
		t.Fatal("run never reached its first lookup")
	}
}

func do(t *testing.T, srv *server.Server, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("body missing %q:\n%s", w, body)
		}
	}
}

func validSettingsForm() url.Values {
	return url.Values{
		"enabled": {"on"}, "weekday": {"3"}, "hour_utc": {"7"}, "min_missing": {"2"}, "max_grabs": {"4"},
		"max_lookups": {"40"}, "max_searches": {"10"}, "anime": {"on"}, "excluded_series": {"2"},
	}
}

func TestMaintenance_RendersTabsAndCard(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)

	rec := do(t, env.srv, http.MethodGet, "/media/maintenance", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mustContain(t, rec.Body.String(),
		`href="/media/"`, `href="/media/maintenance"`, `href="/media/selftest"`,
		`name="max_grabs" value="5"`, `name="max_lookups" value="60"`, `name="hour_utc" value="12"`,
		`name="max_searches" value="0"`, "not counted in max grabs",
		"Severance", "Andor", `name="excluded_series" value="2"`,
		"Run now", "Dry run")
}

func TestMaintenance_FlagWarning(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)

	body := do(t, env.srv, http.MethodGet, "/media/maintenance", nil).Body.String()
	mustContain(t, body, "autoRedownloadFailed: on", "autoRedownloadFailedFromInteractiveSearch: off")
	if strings.Contains(body, "the sweep refuses to run") {
		t.Errorf("warning shown with the interactive flag off:\n%s", body)
	}
}

func TestDashboardIndex_HasTabBar(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)

	rec := do(t, srv, http.MethodGet, "/media/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), `href="/media/"`, `href="/media/maintenance"`, `href="/media/selftest"`)
}

func TestMaintenanceSettings_SaveValid(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)

	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/pack-sweep/settings", validSettingsForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got, err := sweep.LoadSettings(context.Background(), env.db)
	if err != nil {
		t.Fatal(err)
	}
	want := sweep.Settings{
		Enabled: true, Weekday: 3, HourUTC: 7, MinMissing: 2, MaxGrabs: 4, MaxLookups: 40, MaxSearches: 10,
		Anime: true, ExcludedSeries: []int{2},
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("stored %s, want %s", gotJSON, wantJSON)
	}
	mustContain(t, rec.Body.String(), "Saved")
}

func TestMaintenanceSettings_Invalid(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)
	form := validSettingsForm()
	form.Set("max_grabs", "0")
	form.Set("hour_utc", "24")

	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/pack-sweep/settings", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(),
		"must be between 1 and 50", "must be between 0 and 23", `name="max_grabs" value="0"`)
	if _, err := env.db.GetSetting(context.Background(), sweep.SettingsKey); err == nil {
		t.Error("invalid settings were stored")
	}
}

func TestMaintenance_CrossOriginPostRejected(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/media/maintenance/pack-sweep/settings",
		strings.NewReader(validSettingsForm().Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", rec.Code)
	}
	if _, err := env.db.GetSetting(context.Background(), sweep.SettingsKey); err == nil {
		t.Error("cross-site POST stored settings")
	}
}

func TestMaintenanceRun_Refusals(t *testing.T) {
	t.Parallel()

	t.Run("active", func(t *testing.T) {
		t.Parallel()
		env := newMaintenanceTestServer(t)
		env.startHeldRun(t)
		rec := do(t, env.srv, http.MethodPost, "/media/maintenance/pack-sweep/run", url.Values{"dry": {"1"}})
		mustContain(t, rec.Body.String(), "a run is already active")
	})

	t.Run("paused", func(t *testing.T) {
		t.Parallel()
		env := newMaintenanceTestServer(t)
		enabled := sweep.DefaultSettings()
		enabled.Enabled = true
		if err := sweep.SaveSettings(context.Background(), env.db, enabled); err != nil {
			t.Fatal(err)
		}
		if err := env.db.SetSetting(context.Background(), "autonomous_paused", "true"); err != nil {
			t.Fatal(err)
		}
		rec := do(t, env.srv, http.MethodPost, "/media/maintenance/pack-sweep/run", url.Values{})
		mustContain(t, rec.Body.String(), "paused")
		if runs, _ := env.db.ListSweepRuns(context.Background(), 10); len(runs) != 0 {
			t.Errorf("paused Run now created %d runs", len(runs))
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		env := newMaintenanceTestServer(t)
		rec := do(t, env.srv, http.MethodPost, "/media/maintenance/pack-sweep/run", url.Values{})
		mustContain(t, rec.Body.String(), "disabled")
		if runs, _ := env.db.ListSweepRuns(context.Background(), 10); len(runs) != 0 {
			t.Errorf("disabled Run now created %d runs", len(runs))
		}
	})
}

func TestMaintenanceRun_StartsDryRun(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)

	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/pack-sweep/run", url.Values{"dry": {"1"}})
	mustContain(t, rec.Body.String(), "Dry run started", `hx-trigger="every 10s"`)
	select {
	case <-env.sonarr.lookups:
	case <-time.After(5 * time.Second):
		t.Fatal("dry run never looked anything up")
	}
	runs, err := env.db.ListSweepRuns(context.Background(), 1)
	if err != nil || len(runs) != 1 || !runs[0].Dry || runs[0].Trigger != "manual" {
		t.Fatalf("runs = %+v, err %v", runs, err)
	}
}

func TestMaintenanceStatus_Fragment(t *testing.T) {
	t.Parallel()

	t.Run("running", func(t *testing.T) {
		t.Parallel()
		env := newMaintenanceTestServer(t)
		env.startHeldRun(t)
		body := do(t, env.srv, http.MethodGet, "/media/maintenance/pack-sweep/status", nil).Body.String()
		mustContain(t, body, "season pass", "Severance S01", "lookups 1/60", "grabs 0/5", "searches 0/0",
			`hx-trigger="every 10s"`)
	})

	t.Run("idle", func(t *testing.T) {
		t.Parallel()
		env := newMaintenanceTestServer(t)
		set := sweep.DefaultSettings()
		set.Enabled, set.Weekday, set.HourUTC = true, 2, 9
		if err := sweep.SaveSettings(context.Background(), env.db, set); err != nil {
			t.Fatal(err)
		}
		body := do(t, env.srv, http.MethodGet, "/media/maintenance/pack-sweep/status", nil).Body.String()
		next := env.sch.NextRun(time.Now())
		if next == nil {
			t.Fatal("NextRun nil with the sweep enabled")
		}
		mustContain(t, body, "Idle", next.Format("Mon Jan 2 15:04 UTC"))
		if strings.Contains(body, "hx-trigger") {
			t.Errorf("idle fragment polls:\n%s", body)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		env := newMaintenanceTestServer(t)
		body := do(t, env.srv, http.MethodGet, "/media/maintenance/pack-sweep/status", nil).Body.String()
		mustContain(t, body, "Idle", "not scheduled (disabled)")
	})
}

func TestMaintenance_RunsAndItemsListed(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)
	ctx := context.Background()
	id, err := env.db.CreateSweepRun(ctx, "manual", true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	season, n, ms := 2, 1234, 56789
	if err = env.db.AddSweepItem(ctx, &db.SweepItem{
		RunID: id, Target: "s:1:2", SeriesID: 1, SeriesTitle: "Severance", Season: &season, Episodes: "1-10",
		ReleaseTitle: "Severance.S02.1080p", Indexer: "Zilean", Result: "would_grab",
		LookupResults: &n, LookupMS: &ms, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	counts := db.SweepCounts{Lookups: 1, MaxGrabs: 5}
	if err = env.db.FinishSweepRun(ctx, id, "done", "", counts, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := do(t, env.srv, http.MethodGet, "/media/maintenance", nil).Body.String()
	mustContain(t, body, "<details", "dry", "done", "Severance", "S02", "Severance.S02.1080p", "Zilean",
		"would_grab", "1234", "56.8s")
}

func TestMaintenanceForget_DeletesOneRow(t *testing.T) {
	t.Parallel()
	env := newMaintenanceTestServer(t)
	ctx := context.Background()
	now := time.Now()
	keep := &db.SweepRefused{TitleKey: "a", ReleaseTitle: "A.S01", Indexer: "Zilean", RefusedAt: now, Reason: "451"}
	gone := &db.SweepRefused{TitleKey: "b", ReleaseTitle: "B.S01", Indexer: "Comet", RefusedAt: now, Reason: "451"}
	for _, r := range []*db.SweepRefused{keep, gone} {
		if err := env.db.AddSweepRefused(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	mustContain(t, do(t, env.srv, http.MethodGet, "/media/maintenance", nil).Body.String(), "A.S01", "B.S01")

	forget := "/media/maintenance/pack-sweep/refused/" + strconv.FormatInt(gone.ID, 10) + "/forget"
	rec := do(t, env.srv, http.MethodPost, forget, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	left, err := env.db.ListSweepRefused(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != keep.ID {
		t.Errorf("left = %+v, want only %d", left, keep.ID)
	}
	bad := do(t, env.srv, http.MethodPost, "/media/maintenance/pack-sweep/refused/x/forget", url.Values{})
	if bad.Code != http.StatusBadRequest {
		t.Errorf("bad id status %d, want 400", bad.Code)
	}
}

func TestMaintenance_NoSweepConfigured(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)

	rec := do(t, srv, http.MethodGet, "/media/maintenance", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "sweep unavailable (Sonarr not configured)", `href="/media/"`)
	for _, p := range []string{"settings", "run", "refused/1/forget"} {
		got := do(t, srv, http.MethodPost, "/media/maintenance/pack-sweep/"+p, url.Values{})
		if got.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s status %d, want 503", p, got.Code)
		}
	}
	got := do(t, srv, http.MethodGet, "/media/maintenance/pack-sweep/status", nil)
	if got.Code != http.StatusServiceUnavailable {
		t.Errorf("GET status %d, want 503", got.Code)
	}
}
