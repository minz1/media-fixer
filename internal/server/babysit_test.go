package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/babysit"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/server"
	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/sweep"
)

const hostile = "<script>alert(1)</script> **bold**"

type bSonarr struct {
	mu      sync.Mutex
	reqs    int
	down    bool
	history []map[string]any
}

func (f *bSonarr) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

func (f *bSonarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs++
	down, history := f.down, f.history
	f.mu.Unlock()
	if down {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	aired := time.Now().Add(-48 * time.Hour)
	var body any = []any{}
	switch r.URL.Path {
	case "/api/v3/series":
		body = []client.Series{
			{ID: 7, Title: "Seven", Monitored: true, SeriesType: "standard"},
			{ID: 8, Title: "Eight", Monitored: true, SeriesType: "standard"},
		}
	case "/api/v3/series/7":
		body = client.Series{ID: 7, Title: "Seven", Monitored: true, SeriesType: "standard"}
	case "/api/v3/series/8":
		body = client.Series{ID: 8, Title: "Eight", Monitored: true, SeriesType: "standard"}
	case "/api/v3/episode":
		body = []client.Episode{
			{ID: 1, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 1, Monitored: true, AirDateUtc: &aired},
			{ID: 2, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 2, Monitored: true, HasFile: true, AirDateUtc: &aired},
		}
	case "/api/v3/history/series":
		body = history
	case "/api/v3/config/downloadclient":
		body = client.DownloadClientConfig{AutoRedownloadFailed: true}
	default:
		if strings.HasPrefix(r.URL.Path, "/api/v3/series/") {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

type bLLM struct {
	mu    sync.Mutex
	calls int
	reply string
	err   error
}

func (l *bLLM) Complete(context.Context, string, string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.reply, l.err
}

func (l *bLLM) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type bEnv struct {
	srv   *server.Server
	db    *db.DB
	sch   *sweep.Scheduler
	son   *bSonarr
	llm   *bLLM
	queue *stuckqueue.Result
}

func newBabysitTestServer(t *testing.T) *bEnv {
	t.Helper()
	srv, database := newDashboardTestServer(t)
	son := &bSonarr{}
	ts := httptest.NewServer(son)
	t.Cleanup(ts.Close)
	arr := client.NewArr(ts.URL, "k")

	ctx, cancel := context.WithCancel(context.Background())
	r := &sweep.Runner{
		Sonarr: arr, DB: database, Now: time.Now,
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
	sch := sweep.NewScheduler(ctx, r, database)
	t.Cleanup(func() {
		cancel()
		sch.Wait()
	})
	srv.SetSweep(sch)

	e := &bEnv{srv: srv, db: database, sch: sch, son: son, llm: &bLLM{reply: "indexers have nothing"}}
	e.queue = &stuckqueue.Result{}
	srv.SetBabysit(&babysit.Service{
		DB: database, Sonarr: arr, LLM: e.llm, Model: "m1", Now: time.Now,
		State: func() (stuckqueue.Result, bool, bool) { return *e.queue, true, false },
	})
	return e
}

func (e *bEnv) enableSweep(t *testing.T) {
	t.Helper()
	set := sweep.DefaultSettings()
	set.Enabled = true
	if err := sweep.SaveSettings(context.Background(), e.db, set, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func (e *bEnv) stuckSeven(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	err := e.db.UpsertSweepTarget(ctx, db.SweepTarget{
		Target: "s:7:1", SeriesID: 7, SeriesTitle: "Seven", Season: 1, Streak: 9, Missing: 1, Attempts: 9,
		LastAttempt: time.Now().Add(-time.Hour), NextEligible: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := e.db.CreateSweepRun(ctx, "schedule", false, time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	season := 1
	err = e.db.AddSweepItem(ctx, &db.SweepItem{
		RunID: run, Target: "s:7:1", SeriesID: 7, SeriesTitle: "Seven", Season: &season,
		ReleaseTitle: hostile, Indexer: "Nyaa", Result: "grabbed", At: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.son.history = []map[string]any{{
		"id": 1, "date": time.Now().Add(-48 * time.Hour).Format(time.RFC3339), "eventType": "grabbed",
		"sourceTitle": "Seven.S01", "episode": map[string]any{"seasonNumber": 1, "episodeNumber": 1},
		"data": map[string]any{"indexer": "Nyaa"},
	}}
}

func (e *bEnv) waitIdle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		e.sch.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run is still going 5s later")
	}
}

func (e *bEnv) runs(t *testing.T) []db.SweepRun {
	t.Helper()
	runs, err := e.db.ListSweepRuns(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (e *bEnv) settingsRaw(t *testing.T) string {
	t.Helper()
	raw, _ := e.db.GetSetting(context.Background(), sweep.SettingsKey)
	return raw
}

func (e *bEnv) excluded(t *testing.T) []int {
	t.Helper()
	set, err := sweep.LoadSettings(context.Background(), e.db)
	if err != nil {
		t.Fatal(err)
	}
	return set.ExcludedSeries
}

func doWith(
	t *testing.T, srv *server.Server, method, path string, form url.Values, hdr map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestBabysit_TabOnEveryPage(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	for _, p := range []string{"/media/", "/media/maintenance", "/media/babysit", "/media/babysit/7"} {
		rec := do(t, e.srv, http.MethodGet, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", p, rec.Code)
		}
		mustContain(t, rec.Body.String(), `href="/media/babysit"`, "Babysit")
	}
}

func TestBabysit_NotConfigured(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)
	rec := do(t, srv, http.MethodGet, "/media/babysit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "sweep unavailable (Sonarr not configured)", `href="/media/babysit"`)
	for _, p := range []string{
		"/media/babysit/7/search/packs", "/media/babysit/7/explain", "/media/babysit/exclude", "/media/babysit/include",
	} {
		got := do(t, srv, http.MethodPost, p, url.Values{"series_id": {"7"}})
		if got.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s: %d, want 503", p, got.Code)
		}
	}
	if got := do(t, srv, http.MethodGet, "/media/babysit/7", nil); got.Code != http.StatusServiceUnavailable {
		t.Errorf("GET show: %d, want 503", got.Code)
	}
}

func TestBabysit_ListRenders(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	if err := sweep.SetExcluded(context.Background(), e.db, 8, true); err != nil {
		t.Fatal(err)
	}
	body := do(t, e.srv, http.MethodGet, "/media/babysit", nil).Body.String()
	mustContain(t, body, `href="/media/babysit/7"`, "Seven", "Eight", "grabbed", "&lt;script&gt;",
		`hx-confirm="Include`, `hx-confirm="Exclude`, `<option value="7"`, `name="series_id"`)
	if strings.Contains(body, `<option value="8"`) {
		t.Error("an excluded series is in the picker")
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("unescaped release title")
	}
}

func TestBabysit_ListSonarrDown(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	e.son.down = true
	rec := do(t, e.srv, http.MethodGet, "/media/babysit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	mustContain(t, rec.Body.String(), "Seven", "Sonarr could not be read")
	if strings.Contains(rec.Body.String(), "<option") {
		t.Error("picker shown with Sonarr down")
	}
}

func TestBabysit_ShowRenders(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	item := stuckqueue.Item{Verdict: stuckqueue.VerdictGap}
	item.App, item.DownloadID, item.Title = stuckqueue.AppSonarr, "d1", "Seven.S01E02"
	item.Records = []client.QueueEntry{{SeriesID: 7, Messages: []string{"no files <i>found"}}}
	e.queue.Apps = []stuckqueue.AppResult{{App: stuckqueue.AppSonarr, Configured: true, Items: []stuckqueue.Item{item}}}
	body := do(t, e.srv, http.MethodGet, "/media/babysit/7", nil).Body.String()
	mustContain(t, body, "Seven", "stuck", "S01", "Nyaa", "&lt;script&gt;", "**bold**", "Seven.S01E02",
		"no files &lt;i&gt;found",
		`hx-post="/media/babysit/7/search/packs"`, `hx-post="/media/babysit/7/search/singles"`,
		`hx-post="/media/babysit/exclude"`, `hx-post="/media/babysit/7/explain"`,
		"left today", "counts as an attempt", "one AI call")
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "<i>found") {
		t.Error("unescaped untrusted text")
	}
}

func TestBabysit_ShowSonarrDown(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	e.son.down = true
	rec := do(t, e.srv, http.MethodGet, "/media/babysit/7", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "Sonarr could not be read", "&lt;script&gt;")
	for _, banned := range []string{"/search/", "/explain"} {
		if strings.Contains(body, banned) {
			t.Errorf("page offers %q with Sonarr down", banned)
		}
	}
}

func TestBabysit_BadSeriesID(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	for _, id := range []string{"abc", "0", "-1"} {
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/media/babysit/" + id},
			{http.MethodPost, "/media/babysit/" + id + "/search/packs"},
			{http.MethodPost, "/media/babysit/" + id + "/explain"},
		} {
			if rec := do(t, e.srv, c.method, c.path, url.Values{}); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d, want 404", c.method, c.path, rec.Code)
			}
		}
	}
	if n := e.son.requests(); n != 0 {
		t.Errorf("%d Sonarr requests for bad ids", n)
	}
	if rec := do(t, e.srv, http.MethodGet, "/media/babysit/99", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown series: %d, want 404", rec.Code)
	}
}

func TestBabysit_Search(t *testing.T) {
	t.Parallel()
	for _, pass := range []string{"packs", "singles"} {
		t.Run(pass, func(t *testing.T) {
			t.Parallel()
			e := newBabysitTestServer(t)
			e.enableSweep(t)
			rec := do(t, e.srv, http.MethodPost, "/media/babysit/7/search/"+pass, url.Values{})
			mustContain(t, rec.Body.String(), "/media/maintenance/pack-sweep/status", `hx-trigger="every 10s"`)
			e.waitIdle(t)
			runs := e.runs(t)
			if len(runs) != 1 || runs[0].Trigger != "babysit: Seven" || runs[0].Dry {
				t.Fatalf("runs = %+v", runs)
			}
		})
	}
}

func TestBabysit_SearchBogusPass(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.enableSweep(t)
	rec := do(t, e.srv, http.MethodPost, "/media/babysit/7/search/bogus", url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
	if n := len(e.runs(t)); n != 0 {
		t.Errorf("%d runs started", n)
	}
}

func TestBabysit_SearchRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*testing.T, *bEnv)
		want  string
	}{
		{"disabled", func(*testing.T, *bEnv) {}, "disabled"},
		{"excluded", func(t *testing.T, e *bEnv) {
			e.enableSweep(t)
			if err := sweep.SetExcluded(context.Background(), e.db, 7, true); err != nil {
				t.Fatal(err)
			}
		}, "excluded"},
		{"corrupt", func(t *testing.T, e *bEnv) {
			if err := e.db.SetSetting(context.Background(), sweep.SettingsKey, "{nope"); err != nil {
				t.Fatal(err)
			}
		}, "unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newBabysitTestServer(t)
			c.setup(t, e)
			before := e.settingsRaw(t)
			rec := do(t, e.srv, http.MethodPost, "/media/babysit/7/search/packs", url.Values{})
			mustContain(t, rec.Body.String(), c.want)
			if strings.Contains(rec.Body.String(), `hx-trigger="every 10s"`) {
				t.Error("status polling started for a refused search")
			}
			if n := len(e.runs(t)); n != 0 {
				t.Errorf("%d runs started", n)
			}
			if e.settingsRaw(t) != before {
				t.Error("settings changed")
			}
		})
	}
}

func TestBabysit_ExcludeInclude(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	form := url.Values{"series_id": {"7"}}

	rec := do(t, e.srv, http.MethodPost, "/media/babysit/exclude", form)
	if rec.Code != http.StatusOK {
		t.Fatalf("exclude: %d", rec.Code)
	}
	if got := e.excluded(t); len(got) != 1 || got[0] != 7 {
		t.Fatalf("excluded = %v", got)
	}
	form.Set("view", "show")
	rec = do(t, e.srv, http.MethodPost, "/media/babysit/include", form)
	mustContain(t, rec.Body.String(), "Seven")
	if got := e.excluded(t); len(got) != 0 {
		t.Fatalf("excluded = %v", got)
	}
}

func TestBabysit_ExcludeBadSeriesID(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.enableSweep(t)
	before := e.settingsRaw(t)
	for _, id := range []string{"0", "-3", "abc", ""} {
		for _, p := range []string{"exclude", "include"} {
			rec := do(t, e.srv, http.MethodPost, "/media/babysit/"+p, url.Values{"series_id": {id}})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s id %q: %d, want 400", p, id, rec.Code)
			}
		}
	}
	if e.settingsRaw(t) != before {
		t.Error("settings changed")
	}
}

func TestBabysit_ExcludeCorruptSettings(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	if err := e.db.SetSetting(context.Background(), sweep.SettingsKey, "{nope"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"exclude", "include"} {
		rec := do(t, e.srv, http.MethodPost, "/media/babysit/"+p, url.Values{"series_id": {"7"}})
		mustContain(t, rec.Body.String(), "unreadable")
		if e.settingsRaw(t) != "{nope" {
			t.Fatalf("%s overwrote the corrupt row", p)
		}
	}
}

func TestBabysit_ExplainRendersEscapedAndCaches(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	e.llm.reply = hostile

	rec := do(t, e.srv, http.MethodPost, "/media/babysit/7/explain", url.Values{})
	body := rec.Body.String()
	mustContain(t, body, "&lt;script&gt;alert(1)&lt;/script&gt; **bold**")
	if strings.Contains(body, "<script>alert") {
		t.Fatal("model output rendered unescaped")
	}
	if e.llm.count() != 1 {
		t.Fatalf("llm calls = %d", e.llm.count())
	}

	fresh := do(t, e.srv, http.MethodGet, "/media/babysit/7", nil).Body.String()
	mustContain(t, fresh, "&lt;script&gt;alert(1)&lt;/script&gt; **bold**")
	if strings.Contains(fresh, "/explain") || strings.Contains(fresh, "history changed since") {
		t.Error("fresh cache offers a button or says stale")
	}

	e.son.mu.Lock()
	e.son.history = append(e.son.history, map[string]any{
		"id": 2, "date": time.Now().Add(-time.Hour).Format(time.RFC3339), "eventType": "downloadFailed",
		"sourceTitle": "Seven.S01", "data": map[string]any{},
	})
	e.son.mu.Unlock()
	stale := do(t, e.srv, http.MethodGet, "/media/babysit/7", nil).Body.String()
	mustContain(t, stale, "history changed since", `hx-post="/media/babysit/7/explain"`, "&lt;script&gt;")
}

func TestBabysit_ExplainRefusedWhenNotStuck(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	rec := do(t, e.srv, http.MethodPost, "/media/babysit/8/explain", url.Values{})
	mustContain(t, rec.Body.String(), "not stuck")
	if e.llm.count() != 0 {
		t.Error("llm called for a show that is not stuck")
	}
	if strings.Contains(do(t, e.srv, http.MethodGet, "/media/babysit/8", nil).Body.String(), "/explain") {
		t.Error("Explain offered for a show that is not stuck")
	}
}

func TestBabysit_ExplainErrorShownNotCached(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	e.llm.err = errors.New("model on fire")
	rec := do(t, e.srv, http.MethodPost, "/media/babysit/7/explain", url.Values{})
	mustContain(t, rec.Body.String(), "model on fire")
	if _, err := e.db.GetBabysitExplanation(context.Background(), 7); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("an explanation was cached: %v", err)
	}
}

func TestBabysit_ExplainErrorKeepsCachedAnswer(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.stuckSeven(t)
	if err := e.db.PutBabysitExplanation(context.Background(), db.BabysitExplanation{
		SeriesID: 7, Fingerprint: "old", Text: "OLD ANSWER", Model: "m", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	e.llm.err = errors.New("model on fire")
	rec := do(t, e.srv, http.MethodPost, "/media/babysit/7/explain", url.Values{})
	mustContain(t, rec.Body.String(), "model on fire", "OLD ANSWER", "history changed since")
}

func TestBabysit_ExcludedListStaysOpenAfterSave(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	rec := do(t, e.srv, http.MethodPost, "/media/babysit/exclude", url.Values{"series_id": {"7"}})
	mustContain(t, rec.Body.String(), "<details open>")
}

func TestBabysit_CrossSitePostsRefused(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.enableSweep(t)
	e.stuckSeven(t)
	before := e.settingsRaw(t)
	posts := []string{
		"/media/babysit/7/search/packs", "/media/babysit/7/search/singles", "/media/babysit/7/explain",
		"/media/babysit/exclude", "/media/babysit/include",
	}
	headers := []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example"},
	}
	for _, p := range posts {
		for _, h := range headers {
			rec := doWith(t, e.srv, http.MethodPost, p, url.Values{"series_id": {"7"}}, h)
			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s %v: %d, want 403", p, h, rec.Code)
			}
		}
	}
	if n := len(e.runs(t)); n != 1 {
		t.Errorf("runs = %d, want only the seeded one", n)
	}
	if e.settingsRaw(t) != before || e.llm.count() != 0 {
		t.Error("a refused POST had an effect")
	}
}

func TestBabysit_GetsDoNotMutate(t *testing.T) {
	t.Parallel()
	e := newBabysitTestServer(t)
	e.enableSweep(t)
	e.stuckSeven(t)
	snapshot := func() []any {
		targets, _ := e.db.ListSweepTargets(context.Background())
		_, expErr := e.db.GetBabysitExplanation(context.Background(), 7)
		return []any{len(e.runs(t)), len(targets), expErr == nil, e.settingsRaw(t)}
	}
	before := snapshot()
	for _, p := range []string{"/media/babysit", "/media/babysit/7", "/media/babysit/8", "/media/maintenance"} {
		do(t, e.srv, http.MethodGet, p, nil)
	}
	after := snapshot()
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("state[%d] changed: %v -> %v", i, before[i], after[i])
		}
	}
	if e.llm.count() != 0 {
		t.Error("a GET called the LLM")
	}
}
