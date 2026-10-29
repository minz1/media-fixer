package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/server"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

type stuckArr struct {
	mu      sync.Mutex
	records []map[string]any
	deletes []string
	hold    chan struct{}
}

func (f *stuckArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
		_ = json.NewEncoder(w).Encode(map[string]any{"totalRecords": len(f.records), "records": f.records})
	case r.Method == http.MethodDelete:
		f.deletes = append(f.deletes, r.URL.Path)
		f.records = nil
	default:
		_, _ = w.Write([]byte(`[]`))
	}
}

func (f *stuckArr) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

type stuckEnv struct {
	srv *server.Server
	db  *db.DB
	arr *stuckArr
	sch *stuckqueue.Scheduler
}

func newStuckEnv(t *testing.T) *stuckEnv {
	t.Helper()
	srv, database := newDashboardTestServer(t)
	arr := &stuckArr{records: []map[string]any{
		{
			"id":                   7,
			"downloadId":           "D",
			"title":                "One Piece",
			"status":               "completed",
			"trackedDownloadState": "importBlocked",
		},
	}}
	ts := httptest.NewServer(arr)
	t.Cleanup(ts.Close)
	r := &stuckqueue.Runner{
		Sonarr: client.NewArr(ts.URL, "k"), DB: database, Now: time.Now, Log: slog.New(slog.DiscardHandler),
	}
	sch := stuckqueue.NewScheduler(context.Background(), r, database)
	t.Cleanup(sch.Wait)
	srv.SetStuckQueue(sch)
	return &stuckEnv{srv: srv, db: database, arr: arr, sch: sch}
}

func TestStuckCard_RendersEveryVerdict(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	rec := func(id, ep int, dl string, hasFile bool, msg string) client.QueueEntry {
		return client.QueueEntry{
			ID: id, DownloadID: dl, Status: "completed", TrackedDownloadState: "importPending", EpisodeID: ep,
			SeasonNumber: 3, EpisodeNumber: ep, HasFile: hasFile, HasFileKnown: true, Messages: []string{msg},
		}
	}
	item := func(r client.QueueEntry, title string, v stuckqueue.Verdict, acts ...stuckqueue.Action) stuckqueue.Item {
		d := stuckqueue.Group(stuckqueue.AppSonarr, []client.QueueEntry{r})[0]
		d.Title = title
		return stuckqueue.Item{Download: d, Verdict: v, Age: 7 * time.Hour, Actions: acts}
	}
	gap := item(rec(1, 11, "LC", false, "XEM mapping not confirmed"), "Link Click", stuckqueue.VerdictGap,
		stuckqueue.ActionImport, stuckqueue.ActionRegrab, stuckqueue.ActionRemove)
	gap.Rejections = []string{"Unable to determine if file is a sample"}
	refused := item(rec(2, 12, "NV", false, "sample"), "NOVA", stuckqueue.VerdictGap,
		stuckqueue.ActionRegrab, stuckqueue.ActionRemove)
	refused.ImportRefusal = "refused: more than one file targets the same episode"
	res := stuckqueue.Result{
		FinishedAt: time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC), DryRun: true,
		Apps: []stuckqueue.AppResult{
			{App: stuckqueue.AppSonarr, Configured: true, WouldClear: 1, Items: []stuckqueue.Item{
				gap,
				refused,
				item(
					rec(3, 13, "FL", true, "Not an upgrade"),
					"Flash",
					stuckqueue.VerdictClear,
					stuckqueue.ActionRemove,
				),
				item(client.QueueEntry{ID: 4, DownloadID: "OP", Status: "completed"}, "One Piece",
					stuckqueue.VerdictUnmapped, stuckqueue.ActionRemove),
			}},
			{App: stuckqueue.AppRadarr, Configured: true, Unreachable: true, Err: "arr GET /api/v3/queue: 500"},
		},
	}
	var buf bytes.Buffer
	data := env.srv.StuckCardDataForTest(res, stuckqueue.DefaultSettings(), "")
	if err := env.srv.ExecuteTemplateForTest(&buf, "stuck_card", data); err != nil {
		t.Fatal(err)
	}
	mustContain(
		t,
		buf.String(),
		"Link Click",
		"NOVA",
		"Flash",
		"One Piece",
		"Import → S03E11",
		"Re-grab",
		"Remove",
		"Unable to determine if file is a sample",
		"more than one file targets the same episode",
		"unreachable: arr GET /api/v3/queue: 500",
		"dry run",
		"would clear 1",
		`name="download_id" value="LC"`,
		`name="targets" value="S03E11"`,
		"/media/maintenance/stuck-queue/import",
	)
}

func TestStuckCard_NotConfigured(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)
	rec := do(t, srv, http.MethodGet, "/media/maintenance", nil)
	mustContain(t, rec.Body.String(), "Stuck queue unavailable (Sonarr and Radarr not configured)")
}

func TestStuckCard_CorruptSettings(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	if err := env.db.SetSetting(context.Background(), stuckqueue.SettingsKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	rec := do(t, env.srv, http.MethodGet, "/media/maintenance", nil)
	mustContain(t, rec.Body.String(), "Stored stuck-queue settings couldn&#39;t be read; saving replaces them.")
}

func TestStuckSettings_SaveAndInvalid(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	ok := url.Values{"enabled": {"on"}, "interval_minutes": {"30"}, "min_age_hours": {"12"}}
	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/stuck-queue/settings", ok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Saved.")
	got, err := stuckqueue.LoadSettings(context.Background(), env.db)
	if err != nil || got != (stuckqueue.Settings{Enabled: true, IntervalMinutes: 30, MinAgeHours: 12}) {
		t.Fatalf("saved = %+v, err = %v", got, err)
	}
	for _, bad := range []string{"abc", "5"} {
		form := url.Values{"enabled": {"on"}, "dry_run": {"on"}, "interval_minutes": {bad}, "min_age_hours": {"6"}}
		rec = do(t, env.srv, http.MethodPost, "/media/maintenance/stuck-queue/settings", form)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Saved.") {
			t.Fatalf("%s: status %d body %s", bad, rec.Code, rec.Body.String())
		}
		if again, _ := stuckqueue.LoadSettings(context.Background(), env.db); again != got {
			t.Fatalf("%s: invalid input was saved: %+v", bad, again)
		}
	}
	mustContain(t, rec.Body.String(), "must be between 15 and 1440")
}

func TestStuckRun_Busy(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	hold := make(chan struct{})
	env.arr.mu.Lock()
	env.arr.hold = hold
	env.arr.mu.Unlock()
	if err := env.sch.RunNow(); err != nil {
		t.Fatal(err)
	}
	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/stuck-queue/run", url.Values{})
	close(hold)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "a stuck-queue run is already running")
}

func TestStuckAction_RequiresSameOrigin(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	form := url.Values{"app": {"sonarr"}, "download_id": {"D"}}
	req := httptest.NewRequest(
		http.MethodPost,
		"/media/maintenance/stuck-queue/remove",
		strings.NewReader(form.Encode()),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || env.arr.deleteCount() != 0 {
		t.Fatalf("status %d, deletes %d", rec.Code, env.arr.deleteCount())
	}
}

func TestStuckAction_RefusalInCard(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	form := url.Values{"app": {"sonarr"}, "download_id": {"GONE"}}
	rec := do(t, env.srv, http.MethodPost, "/media/maintenance/stuck-queue/remove", form)
	if rec.Code != http.StatusOK || env.arr.deleteCount() != 0 {
		t.Fatalf("status %d, deletes %d", rec.Code, env.arr.deleteCount())
	}
	mustContain(t, rec.Body.String(), "no longer in the Sonarr queue")
}

func TestStuckAction_SurvivesCancelledRequest(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	rec := serveCancelled(t, env.srv, "/media/maintenance/stuck-queue/remove?app=sonarr&download_id=D")
	if rec.Code != http.StatusOK || env.arr.deleteCount() != 1 {
		t.Fatalf("status %d, deletes %d, body %s", rec.Code, env.arr.deleteCount(), rec.Body.String())
	}
	mustContain(t, rec.Body.String(), "Removed.")
}

func TestStuckAction_UnknownAction(t *testing.T) {
	t.Parallel()
	env := newStuckEnv(t)
	form := url.Values{"app": {"sonarr"}, "download_id": {"D"}}
	if rec := do(
		t,
		env.srv,
		http.MethodPost,
		"/media/maintenance/stuck-queue/explode",
		form,
	); rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
}
