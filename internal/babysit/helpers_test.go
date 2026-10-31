package babysit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/babysit"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func now0() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }

type fakeSonarr struct {
	mu            sync.Mutex
	series        []client.Series
	episodes      map[int][]client.Episode
	queue         []client.QueueDetail
	history       map[int][]map[string]any
	listStatus    int
	seriesStatus  map[int]int
	historyStatus int
	hits          map[string]int
}

func (f *fakeSonarr) hit(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name]
}

func (f *fakeSonarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.hits[path]++
	var body any
	status := http.StatusOK
	switch {
	case path == "/api/v3/series":
		body, status = f.series, orOK(f.listStatus)
	case strings.HasPrefix(path, "/api/v3/series/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/api/v3/series/"))
		status = orOK(f.seriesStatus[id])
		for _, s := range f.series {
			if s.ID == id {
				body = s
			}
		}
		if body == nil {
			status = http.StatusNotFound
		}
	case path == "/api/v3/episode":
		id, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		body = f.episodes[id]
	case path == "/api/v3/queue/details":
		body = f.queue
	case path == "/api/v3/history/series":
		id, _ := strconv.Atoi(r.URL.Query().Get("seriesId"))
		body, status = f.history[id], orOK(f.historyStatus)
	default:
		status = http.StatusNotFound
	}
	w.WriteHeader(status)
	if status == http.StatusOK {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func orOK(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

type fakeLLM struct {
	mu       sync.Mutex
	calls    int
	system   string
	user     string
	deadline time.Time
	ctxErr   error
	reply    string
	err      error
	enter    chan struct{}
	release  chan struct{}
}

func (l *fakeLLM) Complete(ctx context.Context, system, user string) (string, error) {
	l.mu.Lock()
	l.calls++
	l.system, l.user = system, user
	l.deadline, _ = ctx.Deadline()
	reply, err := l.reply, l.err
	enter, release := l.enter, l.release
	l.mu.Unlock()
	if enter != nil {
		enter <- struct{}{}
		<-release
	}
	l.mu.Lock()
	l.ctxErr = ctx.Err()
	l.mu.Unlock()
	return reply, err
}

func (l *fakeLLM) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type env struct {
	t     *testing.T
	svc   *babysit.Service
	db    *db.DB
	son   *fakeSonarr
	llm   *fakeLLM
	state stuckqueue.Result
	has   bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	son := &fakeSonarr{
		episodes: map[int][]client.Episode{}, history: map[int][]map[string]any{},
		seriesStatus: map[int]int{}, hits: map[string]int{},
	}
	srv := httptest.NewServer(son)
	t.Cleanup(srv.Close)
	e := &env{t: t, db: d, son: son, llm: &fakeLLM{reply: "because"}, has: true}
	e.svc = &babysit.Service{
		DB: d, Sonarr: client.NewArr(srv.URL, "k"), LLM: e.llm, Model: "m1",
		Now:   now0,
		State: func() (stuckqueue.Result, bool, bool) { return e.state, e.has, false },
	}
	return e
}

func (e *env) addSeries(id int, title string) {
	e.son.series = append(e.son.series, client.Series{
		ID: id, Title: title, Monitored: true, SeriesType: "standard", Added: now0().AddDate(0, -1, 0),
	})
}

func (e *env) target(id int, title string, streak, missing int) {
	e.t.Helper()
	err := e.db.UpsertSweepTarget(context.Background(), db.SweepTarget{
		Target: "s:" + strconv.Itoa(id) + ":1", SeriesID: id, SeriesTitle: title,
		Season: 1, Streak: streak, Missing: missing, Attempts: streak,
		LastAttempt: now0().Add(-time.Hour), NextEligible: now0().Add(time.Hour),
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run(dry bool) int64 {
	e.t.Helper()
	id, err := e.db.CreateSweepRun(context.Background(), "schedule", dry, now0().Add(-2*time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) item(run int64, id int, title, release string, at time.Time) {
	e.t.Helper()
	season := 1
	it := &db.SweepItem{
		RunID: run, Target: "s:" + strconv.Itoa(id) + ":1", SeriesID: id, SeriesTitle: title, Season: &season,
		ReleaseTitle: release, Indexer: "Nyaa", Result: "grabbed", At: at,
	}
	if err := e.db.AddSweepItem(context.Background(), it); err != nil {
		e.e(err)
	}
}

func (e *env) e(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func queueItem(seriesID int, title string, msgs ...string) stuckqueue.Item {
	return stuckqueue.Item{
		App: stuckqueue.AppSonarr, DownloadID: "dl-" + title, Title: title,
		Records: []client.QueueEntry{{SeriesID: seriesID, Messages: msgs, Title: title}},
		Verdict: stuckqueue.VerdictGap,
	}
}

func aired(id, season, ep int, hasFile bool) client.Episode {
	at := now0().AddDate(0, 0, -10)
	return client.Episode{
		ID:            id,
		SeriesID:      7,
		SeasonNumber:  season,
		EpisodeNumber: ep,
		HasFile:       hasFile,
		Monitored:     true,
		AirDateUtc:    &at,
	}
}

func histRec(id int, date time.Time, event, source string) map[string]any {
	return map[string]any{
		"id": id, "date": date.Format(time.RFC3339), "eventType": event, "sourceTitle": source,
		"episode": map[string]any{"seasonNumber": 1, "episodeNumber": id % 10},
		"data":    map[string]any{"indexer": "Nyaa", "message": "m" + strconv.Itoa(id)},
	}
}

func (e *env) stuckShow() {
	e.addSeries(7, "Seven")
	e.target(7, "Seven", 3, 2)
	e.son.episodes[7] = []client.Episode{aired(1, 1, 1, true), aired(2, 1, 2, false)}
	e.son.history[7] = []map[string]any{histRec(1, now0().Add(-48*time.Hour), "grabbed", "Seven.S01.1080p")}
}
