package sweep_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func t0() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

type grabPlan struct {
	status   int
	body     string
	grabbed  bool
	outcome  string
	after    int
	imported bool
}

type pendingOutcome struct {
	dlID, title string
	plan        grabPlan
	polls       int
	done        bool
}

type fakeSonarr struct {
	t     *testing.T
	clock *fakeClock
	srv   *httptest.Server

	mu            sync.Mutex
	flags         client.DownloadClientConfig
	series        []client.Series
	episodes      map[int][]client.Episode
	queue         []client.QueueDetail
	history       []client.HistoryRecord
	releases      map[string][]client.Release
	plans         map[string]grabPlan
	pending       []*pendingOutcome
	writes        []string
	lookups       map[string]int
	historyDates  []time.Time
	grabTimes     []time.Time
	seriesDownTil time.Time
	releaseDelay  map[string]time.Duration
	commandStatus map[int]string
	nextID        int
	beforePost    func()
}

func newFakeSonarr(t *testing.T, clock *fakeClock) *fakeSonarr {
	t.Helper()
	f := &fakeSonarr{
		t:             t,
		clock:         clock,
		episodes:      map[int][]client.Episode{},
		releases:      map[string][]client.Release{},
		plans:         map[string]grabPlan{},
		lookups:       map[string]int{},
		releaseDelay:  map[string]time.Duration{},
		commandStatus: map[int]string{},
		nextID:        100,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSonarr) client() *client.ArrClient {
	return client.NewArr(f.srv.URL+"/sonarr", "key")
}

func (f *fakeSonarr) addSeries(id int, title string, eps ...client.Episode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.series = append(f.series, client.Series{ID: id, Title: title, Monitored: true, SeriesType: "standard"})
	f.episodes[id] = append(f.episodes[id], eps...)
}

func (f *fakeSonarr) setReleases(key string, rels ...client.Release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases[key] = rels
}

func (f *fakeSonarr) plan(guid string, p grabPlan) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans[guid] = p
}

func (f *fakeSonarr) writeLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakeSonarr) countWrites(prefix string) int {
	n := 0
	for _, w := range f.writeLog() {
		if strings.HasPrefix(w, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeSonarr) lookupCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups[key]
}

func missingEps(seriesID, season int, ids ...int) []client.Episode {
	aired := t0().Add(-30 * 24 * time.Hour)
	eps := make([]client.Episode, 0, len(ids))
	for i, id := range ids {
		eps = append(eps, client.Episode{
			ID: id, SeriesID: seriesID, SeasonNumber: season, EpisodeNumber: i + 1,
			Monitored: true, AirDateUtc: &aired, AbsoluteEpisodeNumber: id,
		})
	}
	return eps
}

func seasonRel(guid, title, indexer string, weight int) client.Release {
	return client.Release{
		GUID: guid, IndexerID: 1, Title: title, Indexer: indexer, Protocol: "torrent",
		FullSeason: true, ReleaseWeight: weight,
	}
}

func seasonKey(seriesID, season int) string {
	return fmt.Sprintf("s%d:%d", seriesID, season)
}

func (f *fakeSonarr) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/sonarr/api/v3")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && path == "/config/downloadclient":
		f.mu.Lock()
		flags := f.flags
		f.mu.Unlock()
		writeJSON(w, flags)
	case r.Method == http.MethodGet && path == "/series":
		f.mu.Lock()
		down := f.clock.Now().Before(f.seriesDownTil)
		series := f.series
		f.mu.Unlock()
		if down {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, series)
	case r.Method == http.MethodGet && path == "/episode":
		id, _ := strconv.Atoi(q.Get("seriesId"))
		f.mu.Lock()
		eps := f.episodes[id]
		f.mu.Unlock()
		writeJSON(w, eps)
	case r.Method == http.MethodGet && path == "/queue/details":
		f.mu.Lock()
		queue := append([]client.QueueDetail{}, f.queue...)
		f.mu.Unlock()
		writeJSON(w, queue)
	case r.Method == http.MethodGet && path == "/history/since":
		f.serveHistory(w, q)
	case r.Method == http.MethodGet && path == "/release":
		f.serveReleases(w, q)
	case r.Method == http.MethodPost && path == "/release":
		f.serveGrab(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/queue/"):
		f.mu.Lock()
		f.writes = append(f.writes, "DELETE "+path+"?"+r.URL.RawQuery)
		f.mu.Unlock()
	case r.Method == http.MethodPost && path == "/command":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.writes = append(f.writes, "POST /command "+string(b))
		f.mu.Unlock()
		writeJSON(w, map[string]int{"id": id})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/command/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/command/"))
		f.mu.Lock()
		status := f.commandStatus[id]
		f.mu.Unlock()
		if status == "" {
			status = "completed"
		}
		writeJSON(w, map[string]string{"status": status})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSonarr) serveHistory(w http.ResponseWriter, q map[string][]string) {
	since, err := time.Parse(time.RFC3339, q["date"][0])
	if err != nil {
		f.t.Errorf("history date %q: %v", q["date"], err)
	}
	f.mu.Lock()
	f.historyDates = append(f.historyDates, since)
	for _, p := range f.pending {
		if p.done {
			continue
		}
		p.polls++
		if p.polls < p.plan.after {
			continue
		}
		p.done = true
		f.applyOutcome(p)
	}
	var out []client.HistoryRecord
	for _, h := range f.history {
		if !h.Date.Before(since) {
			out = append(out, h)
		}
	}
	f.mu.Unlock()
	writeJSON(w, out)
}

func (f *fakeSonarr) applyOutcome(p *pendingOutcome) {
	now := f.clock.Now()
	switch p.plan.outcome {
	case "archive":
		if p.plan.imported {
			f.history = append(f.history, client.HistoryRecord{
				SourceTitle: p.title, DownloadID: p.dlID, EventType: "downloadFolderImported", Date: now,
			})
		}
		rec := client.QueueDetail{ID: 12, DownloadID: p.dlID, TrackedDownloadState: "importPending"}
		rec.StatusMessages = append(rec.StatusMessages, struct {
			Title    string   `json:"title"`
			Messages []string `json:"messages"`
		}{Title: p.title, Messages: []string{"Found archive file, might need to be extracted"}})
		f.queue = append(f.queue, rec)
	case "":
	default:
		f.history = append(f.history, client.HistoryRecord{
			SourceTitle: p.title, DownloadID: p.dlID, EventType: p.plan.outcome, Date: now,
		})
	}
}

func (f *fakeSonarr) serveReleases(w http.ResponseWriter, q map[string][]string) {
	var key string
	if ep := q["episodeId"]; len(ep) > 0 {
		key = "e" + ep[0]
	} else {
		key = "s" + q["seriesId"][0] + ":" + q["seasonNumber"][0]
	}
	f.mu.Lock()
	f.lookups[key]++
	delay := f.releaseDelay[key]
	delete(f.releaseDelay, key)
	rels := f.releases[key]
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if rels == nil {
		rels = []client.Release{}
	}
	writeJSON(w, rels)
}

func (f *fakeSonarr) serveGrab(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GUID string `json:"guid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	hook := f.beforePost
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.clock.Now()
	f.writes = append(f.writes, "POST /release "+body.GUID)
	f.grabTimes = append(f.grabTimes, now)
	p, ok := f.plans[body.GUID]
	if !ok {
		p = grabPlan{grabbed: true}
	}
	title := ""
	for _, rels := range f.releases {
		for _, rel := range rels {
			if rel.GUID == body.GUID {
				title = rel.Title
			}
		}
	}
	if p.grabbed {
		dlID := "DL-" + strings.ToUpper(body.GUID)
		f.history = append(f.history, client.HistoryRecord{
			SourceTitle: title, DownloadID: dlID, EventType: "grabbed", Date: now,
		})
		f.pending = append(f.pending, &pendingOutcome{dlID: strings.ToLower(dlID), title: title, plan: p})
	}
	if p.status != 0 {
		http.Error(w, p.body, p.status)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type harness struct {
	fake   *fakeSonarr
	clock  *fakeClock
	db     *db.DB
	runner *sweep.Runner
	dms    *[]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clock := &fakeClock{t: t0()}
	fake := newFakeSonarr(t, clock)
	d, err := db.Open(filepath.Join(t.TempDir(), "sweep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var dmMu sync.Mutex
	dms := &[]string{}
	r := &sweep.Runner{
		Sonarr: fake.client(),
		DB:     d,
		Notify: func(_ context.Context, msg string) error {
			dmMu.Lock()
			defer dmMu.Unlock()
			*dms = append(*dms, msg)
			return nil
		},
		Now:   clock.Now,
		Sleep: clock.Sleep,
	}
	return &harness{fake: fake, clock: clock, db: d, runner: r, dms: dms}
}

func (h *harness) run(t *testing.T, dry bool, s sweep.Settings) db.SweepRun {
	t.Helper()
	ctx := context.Background()
	id, err := h.runner.Run(ctx, "manual", dry, s)
	if err != nil {
		t.Logf("run error: %v", err)
	}
	runs, lerr := h.db.ListSweepRuns(ctx, 10)
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, run := range runs {
		if run.ID == id {
			return run
		}
	}
	t.Fatalf("run %d not stored", id)
	return db.SweepRun{}
}

func (h *harness) setPaused(t *testing.T, paused bool) {
	t.Helper()
	if err := h.db.SetSetting(context.Background(), "autonomous_paused", strconv.FormatBool(paused)); err != nil {
		t.Fatal(err)
	}
}

func itemResults(run db.SweepRun) []string {
	out := make([]string, 0, len(run.Items))
	for _, it := range run.Items {
		out = append(out, it.Result)
	}
	return out
}
