package stuckqueue_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const (
	sample    = "Unable to determine if file is a sample"
	cfUpgrade = "Not a Custom Format upgrade for existing episode file(s). New: [v2]"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type runEnv struct {
	sonarr, radarr *fakeArr
	db             *db.DB
	dbPath         string
	clock          *fakeClock
	logs           *syncBuffer
	runner         *stuckqueue.Runner
	mu             sync.Mutex
	dms            []string
	notifyErr      error
}

func newRunEnv(t *testing.T) *runEnv {
	t.Helper()
	e := &runEnv{
		sonarr: newFakeArr(t), radarr: newFakeArr(t), logs: &syncBuffer{},
		clock:  &fakeClock{now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)},
		dbPath: filepath.Join(t.TempDir(), "stuck.db"),
	}
	d, err := db.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	e.db = d
	e.runner = &stuckqueue.Runner{
		Sonarr: e.sonarr.client(), Radarr: e.radarr.client(), DB: d, Now: e.clock.Now,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)),
		Notify: func(_ context.Context, msg string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.notifyErr != nil {
				return e.notifyErr
			}
			e.dms = append(e.dms, msg)
			return nil
		},
	}
	return e
}

func (e *runEnv) dmsCopy() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.dms...)
}

func liveSettings() stuckqueue.Settings {
	return stuckqueue.Settings{Enabled: true, DryRun: false, IntervalMinutes: 60, MinAgeHours: 6}
}

func (e *runEnv) agedRun(set stuckqueue.Settings) stuckqueue.Result {
	e.runner.Run(context.Background(), set)
	e.clock.Add(6 * time.Hour)
	return e.runner.Run(context.Background(), set)
}

func deletes(ws []write) []write {
	return slices.DeleteFunc(ws, func(w write) bool { return w.Method != http.MethodDelete })
}

func app(res stuckqueue.Result, a stuckqueue.App) stuckqueue.AppResult {
	for _, ar := range res.Apps {
		if ar.App == a {
			return ar
		}
	}
	return stuckqueue.AppResult{}
}

func TestRun_WaitsMinAge(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(1, "D1", "Flash", 19, true, notUpgrade))
	e.runner.Run(context.Background(), liveSettings())
	if n := len(deletes(e.sonarr.writesCopy())); n != 0 {
		t.Fatalf("deleted %d on first sight", n)
	}
	e.clock.Add(6 * time.Hour)
	res := e.runner.Run(context.Background(), liveSettings())
	if n := len(deletes(e.sonarr.writesCopy())); n != 1 || app(res, stuckqueue.AppSonarr).Cleared != 1 {
		t.Fatalf("deletes = %d, result = %+v", n, res)
	}
}

func TestRun_DryRun(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(1, "D1", "Flash", 19, true, notUpgrade))
	set := liveSettings()
	set.DryRun = true
	res := e.agedRun(set)
	if n := len(deletes(e.sonarr.writesCopy())); n != 0 || app(res, stuckqueue.AppSonarr).WouldClear != 1 {
		t.Fatalf("deletes = %d, result = %+v", n, res)
	}
	if !res.DryRun {
		t.Fatal("result must say dry run")
	}
}

func TestRun_PausedBlocksClearsNotDM(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	if err := e.db.SetSetting(context.Background(), "autonomous_paused", "true"); err != nil {
		t.Fatal(err)
	}
	e.sonarr.set(rawRec(1, "D1", "Flash", 19, true, notUpgrade), rawRec(2, "D2", "NOVA", 1, false, sample))
	res := e.agedRun(liveSettings())
	if n := len(deletes(e.sonarr.writesCopy())); n != 0 || app(res, stuckqueue.AppSonarr).WouldClear != 1 {
		t.Fatalf("deletes = %d, result = %+v", n, res)
	}
	if dms := e.dmsCopy(); len(dms) != 1 || !strings.Contains(dms[0], "NOVA") {
		t.Fatalf("dms = %q", dms)
	}
}

func TestRun_PauseErrorIsPaused(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	raw, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.ExecContext(
		context.Background(),
		`DELETE FROM settings WHERE key = 'autonomous_paused'`,
	); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	e.sonarr.set(rawRec(1, "D1", "Flash", 19, true, notUpgrade))
	e.agedRun(liveSettings())
	if n := len(deletes(e.sonarr.writesCopy())); n != 0 {
		t.Fatalf("deleted %d while the pause state was unknown", n)
	}
}

func TestRun_RemovalFlags(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(1, "D1", "Pack", 1, true, notUpgrade), rawRec(2, "D1", "Pack", 2, true, notUpgrade))
	e.agedRun(liveSettings())
	ds := deletes(e.sonarr.writesCopy())
	if len(ds) != 1 || (ds[0].Path != "/api/v3/queue/1" && ds[0].Path != "/api/v3/queue/2") {
		t.Fatalf("deletes = %+v", ds)
	}
	q := ds[0].Query
	if q.Get("removeFromClient") != "false" || q.Get("blocklist") != "false" || q.Get("skipRedownload") != "true" {
		t.Fatalf("query = %v", q)
	}
}

func TestRun_Cap(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	var recs []map[string]any
	for i := range 21 {
		recs = append(recs, rawRec(i+1, fmt.Sprintf("D%d", i), fmt.Sprintf("Show %d", i), i+1, true, notUpgrade))
	}
	e.sonarr.set(recs...)
	res := e.agedRun(liveSettings())
	if n := len(deletes(e.sonarr.writesCopy())); n != 20 || !res.CapHit {
		t.Fatalf("deletes = %d, capHit = %v", n, res.CapHit)
	}
	if dms := e.dmsCopy(); len(dms) != 1 || dms[0] != "📦 Stuck-queue sweep: Stopped after clearing 20 this run." {
		t.Fatalf("dms = %q", dms)
	}
}

func TestRun_CapWithItems(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	recs := []map[string]any{rawRec(100, "GAP", "NOVA", 1, false, sample)}
	for i := range 21 {
		recs = append(recs, rawRec(i+1, fmt.Sprintf("D%d", i), fmt.Sprintf("Show %d", i), i+2, true, notUpgrade))
	}
	e.sonarr.set(recs...)
	e.agedRun(liveSettings())
	if dms := e.dmsCopy(); len(dms) != 1 || !strings.HasSuffix(dms[0], "\nStopped after clearing 20 this run.") {
		t.Fatalf("dms = %q", dms)
	}
}

func TestRun_OneDMPerDownload(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(2, "D2", "NOVA", 1, false, sample))
	e.agedRun(liveSettings())
	e.clock.Add(time.Hour)
	e.runner.Run(context.Background(), liveSettings())
	if dms := e.dmsCopy(); len(dms) != 1 {
		t.Fatalf("dms = %q", dms)
	}
}

func TestRun_FirstRunOneDM(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(
		rawRec(1, "A", "NOVA A", 1, false, sample),
		rawRec(2, "B", "NOVA B", 2, false, sample),
		map[string]any{
			"id": float64(3), "downloadId": "C", "title": "One Piece", "status": "completed",
			"trackedDownloadState": "importBlocked",
		},
	)
	e.agedRun(liveSettings())
	dms := e.dmsCopy()
	if len(dms) != 1 || strings.Count(dms[0], "\n• ") != 3 {
		t.Fatalf("dms = %q", dms)
	}
}

func TestRun_DMLengthLimit(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	var recs []map[string]any
	for i := range 60 {
		recs = append(recs, rawRec(i+1, fmt.Sprintf("D%d", i), strings.Repeat("Long title ", 4)+strconv.Itoa(i), i+1,
			false, sample))
	}
	e.sonarr.set(recs...)
	e.agedRun(liveSettings())
	dms := e.dmsCopy()
	if len(dms) != 1 || len(dms[0]) > 2000 {
		t.Fatalf("dm count %d, len %d", len(dms), len(dms[0]))
	}
	if !strings.Contains(dms[0], "…and ") || !strings.HasSuffix(dms[0], " more") {
		t.Fatalf("dm tail = %q", dms[0][len(dms[0])-40:])
	}
}

func TestRun_Prune(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(2, "D2", "NOVA", 1, false, sample))
	e.runner.Run(context.Background(), liveSettings())
	e.sonarr.set()
	e.clock.Add(time.Hour)
	e.runner.Run(context.Background(), liveSettings())
	e.sonarr.set(rawRec(2, "D2", "NOVA", 1, false, sample))
	e.clock.Add(5 * time.Hour)
	res := e.runner.Run(context.Background(), liveSettings())
	if n := len(app(res, stuckqueue.AppSonarr).Items); n != 0 {
		t.Fatalf("a pruned download came back with its old first-seen time: %d items", n)
	}
}

func TestRun_NilArrSkipped(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.runner.Radarr = nil
	res := e.runner.Run(context.Background(), liveSettings())
	if ar := app(res, stuckqueue.AppRadarr); ar.Configured {
		t.Fatalf("radarr = %+v", ar)
	}
	if strings.Contains(e.logs.String(), "level=WARN") {
		t.Fatalf("logs = %s", e.logs.String())
	}
}

func TestRun_UnreachableArr(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.failQueue = true
	e.radarr.set(rawMovie(1, "M1", "Movie", 5, false, sample))
	res := e.agedRun(liveSettings())
	s, r := app(res, stuckqueue.AppSonarr), app(res, stuckqueue.AppRadarr)
	if !s.Unreachable || s.Err == "" || len(r.Items) != 1 {
		t.Fatalf("sonarr = %+v, radarr = %+v", s, r)
	}
}

func TestRun_Actions(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	unknown := rawRec(3, "UNKNOWN", "Unknown", 3, false, sample)
	delete(unknown, "episodeHasFile")
	e.sonarr.set(
		rawRec(1, "OK", "Importable", 1, false, sample),
		rawRec(2, "HASFILE", "Has file", 2, true, sample),
		unknown,
		map[string]any{"id": float64(4), "downloadId": "UNMAPPED", "title": "Unmapped", "status": "completed",
			"trackedDownloadState": "importBlocked"},
		rawRec(5, "BADGUARD", "Bad guard", 5, false, sample),
	)
	e.sonarr.setCandidates("OK", candidateJSON("OK", 1, false))
	e.sonarr.setCandidates("BADGUARD", candidateJSON("BADGUARD", 5, true))
	res := e.agedRun(liveSettings())
	got := map[string]stuckqueue.Item{}
	for _, it := range app(res, stuckqueue.AppSonarr).Items {
		got[it.DownloadID] = it
	}
	want := map[string][]stuckqueue.Action{
		"OK":       {stuckqueue.ActionImport, stuckqueue.ActionRegrab, stuckqueue.ActionRemove},
		"HASFILE":  {stuckqueue.ActionRemove},
		"UNKNOWN":  {stuckqueue.ActionRemove},
		"UNMAPPED": {stuckqueue.ActionRemove},
		"BADGUARD": {stuckqueue.ActionRegrab, stuckqueue.ActionRemove},
	}
	for dl, acts := range want {
		if !slices.Equal(got[dl].Actions, acts) {
			t.Errorf("%s actions = %v, want %v", dl, got[dl].Actions, acts)
		}
	}
	if got["BADGUARD"].ImportRefusal == "" || got["OK"].ImportRefusal != "" {
		t.Errorf("refusals: ok=%q bad=%q", got["OK"].ImportRefusal, got["BADGUARD"].ImportRefusal)
	}
	if !slices.Contains(got["OK"].Rejections, sample) {
		t.Errorf("rejections = %v", got["OK"].Rejections)
	}
}

func candidateJSON(dl string, ep int, hasFile bool) string {
	return fmt.Sprintf(`[{"path":"/data/downloads/sonarr/%[1]s/f.mkv","series":{"id":1},"seasonNumber":1,
"episodes":[{"id":%[2]d,"seasonNumber":1,"episodeNumber":%[2]d,"hasFile":%[3]t}],"downloadId":%[1]q,
"rejections":[{"reason":%[4]q}]}]`, dl, ep, hasFile, sample)
}

func TestRun_DMFailureRetries(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(2, "D2", "NOVA", 1, false, sample))
	e.notifyErr = errors.New("discord down")
	e.agedRun(liveSettings())
	e.mu.Lock()
	e.notifyErr = nil
	e.mu.Unlock()
	e.clock.Add(time.Hour)
	e.runner.Run(context.Background(), liveSettings())
	if dms := e.dmsCopy(); len(dms) != 1 || !strings.Contains(dms[0], "NOVA") {
		t.Fatalf("dms = %q", dms)
	}
}

func TestRun_ClearNeverInDM(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(1, "D1", "Flash", 19, true, notUpgrade))
	set := liveSettings()
	set.DryRun = true
	res := e.agedRun(set)
	if n := len(app(res, stuckqueue.AppSonarr).Items); n != 1 {
		t.Fatalf("items = %d, want the dry-run Clear listed", n)
	}
	if dms := e.dmsCopy(); len(dms) != 0 {
		t.Fatalf("dms = %q", dms)
	}
}

func TestRun_YoungDownloadNotListed(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	e.sonarr.set(rawRec(2, "D2", "NOVA", 1, false, sample))
	res := e.runner.Run(context.Background(), liveSettings())
	if n := len(app(res, stuckqueue.AppSonarr).Items); n != 0 || len(e.dmsCopy()) != 0 {
		t.Fatalf("items = %d, dms = %q", n, e.dmsCopy())
	}
}

func TestRun_DMText(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	unknown := rawRec(4, "U", "Unknown Show", 4, false, sample)
	delete(unknown, "episodeHasFile")
	e.sonarr.set(
		rawRec(1, "NOVA", "NOVA S09E01", 1, false, sample),
		rawRec(2, "PACK", "Pack", 1, true, notUpgrade), rawRec(3, "PACK", "Pack", 2, false, sample),
		unknown,
		rawRec(5, "AHS", "AHS S13E12", 12, true, sample),
		map[string]any{"id": float64(6), "downloadId": "OP", "title": "One Piece", "status": "completed",
			"trackedDownloadState": "importBlocked"},
	)
	e.radarr.set(
		rawMovie(1, "M1", "Missing Movie", 5, false, sample),
		rawMovie(2, "M2", "Has Movie", 6, true, sample),
		map[string]any{"id": float64(3), "downloadId": "M3", "title": "Skin Like Sun.mp4", "status": "completed",
			"trackedDownloadState": "importBlocked"},
	)
	e.agedRun(liveSettings())
	dms := e.dmsCopy()
	if len(dms) != 1 {
		t.Fatalf("dms = %q", dms)
	}
	want := strings.Join([]string{
		"📦 Stuck downloads need you (Maintenance → Stuck queue):",
		"• Sonarr — NOVA S09E01 — episode missing (S01E01) — blocked by: " + sample,
		"• Sonarr — Pack — episode missing (S01E02) — blocked by: " + sample,
		"• Sonarr — Unknown Show — can't tell if it has a file — blocked by: " + sample,
		"• Sonarr — AHS S13E12 — Sonarr has a file for S01E12 but didn't compare it",
		"• Sonarr — One Piece — not matched to any episode",
		"• Radarr — Missing Movie — movie missing — blocked by: " + sample,
		"• Radarr — Has Movie — Radarr has a file for this movie but didn't compare it",
		"• Radarr — Skin Like Sun.mp4 — not matched to any movie",
	}, "\n")
	if dms[0] != want {
		t.Fatalf("dm =\n%s\nwant\n%s", dms[0], want)
	}
}
