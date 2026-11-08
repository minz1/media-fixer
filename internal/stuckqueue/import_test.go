package stuckqueue_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const (
	pathA = "/data/downloads/sonarr/D/a.mkv"
	pathB = "/data/downloads/sonarr/D/b.mkv"
)

const twoFiles = `[
{"path":"/data/downloads/sonarr/D/a.mkv","series":{"id":1},"seasonNumber":1,"downloadId":"D",
 "quality":{"quality":{"name":"WEBDL-1080p"}},"languages":[{"id":1}],"releaseGroup":"G",
 "episodes":[{"id":1,"seasonNumber":1,"episodeNumber":1}]},
{"path":"/data/downloads/sonarr/D/b.mkv","series":{"id":1},"seasonNumber":1,"downloadId":"D",
 "quality":{"quality":{"name":"WEBDL-1080p"}},"languages":[{"id":1}],"releaseGroup":"G",
 "episodes":[{"id":2,"seasonNumber":1,"episodeNumber":2}]}]`

func importEnv(t *testing.T) (*runEnv, *stuckqueue.Scheduler) {
	t.Helper()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "Show", 1, false, sample), rawRec(2, "D", "Show", 2, false, sample))
	e.sonarr.setCandidates("D", twoFiles)
	e.sonarr.setEpisodes(rawEpisode(1, 1, 1, false), rawEpisode(2, 1, 2, false))
	if err := seedSeen(e, stuckqueue.AppSonarr, "D", e.clock.Now().Add(-7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return e, s
}

func targetsFp(t *testing.T, e *runEnv) string {
	t.Helper()
	entries, err := e.sonarr.client().QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range stuckqueue.Group(stuckqueue.AppSonarr, entries) {
		if d.DownloadID == "D" {
			return d.TargetsFP()
		}
	}
	t.Fatal("download D not in the fake queue")
	return ""
}

func captureRefresh(s *stuckqueue.Scheduler) func() []string {
	var mu sync.Mutex
	var got []string
	s.Refresh = func(subject string) {
		r, _ := s.CardRenderer()(context.Background(), subject)
		up := r.Update
		mu.Lock()
		defer mu.Unlock()
		got = append(got, up.Status)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

func importRows(rows ...stuckqueue.MappedFile) []stuckqueue.MappedFile { return rows }

func bothRows() []stuckqueue.MappedFile {
	return importRows(
		stuckqueue.MappedFile{Path: pathA, EpisodeIDs: []int{1}},
		stuckqueue.MappedFile{Path: pathB, EpisodeIDs: []int{2}},
	)
}

func importMapped(e *runEnv, s *stuckqueue.Scheduler, t *testing.T, rows []stuckqueue.MappedFile) (string, error) {
	t.Helper()
	return s.ImportMapped(context.Background(), 1, "D", targetsFp(t, e), rows)
}

func TestImportMapped_PostsMapping(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	e.sonarr.mu.Lock()
	e.sonarr.writes = nil
	e.sonarr.mu.Unlock()
	statuses := captureRefresh(s)
	notice, err := importMapped(e, s, t, bothRows())
	if err != nil {
		t.Fatal(err)
	}
	if want := "Import started for 2 files (S01E01–S01E02)."; !strings.HasPrefix(notice, want) {
		t.Fatalf("notice = %q, want prefix %q", notice, want)
	}
	ws := e.sonarr.writesCopy()
	if len(ws) != 1 || ws[0].Path != "/api/v3/command" || ws[0].Body["name"] != "ManualImport" ||
		ws[0].Body["importMode"] != "move" {
		t.Fatalf("writes = %+v", ws)
	}
	files, _ := ws[0].Body["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	f1, _ := files[1].(map[string]any)
	if f1["path"] != pathB || f1["seriesId"] != float64(1) || f1["seasonNumber"] != float64(1) ||
		!slices.Equal(f1["episodeIds"].([]any), []any{float64(2)}) {
		t.Fatalf("second file = %v", f1)
	}
	if row, _ := e.rowOf(t, "D"); row.State != string(stuckqueue.StateImportInflight) {
		t.Fatalf("row = %+v, want import_inflight", row)
	}
	if got := statuses(); len(got) != 1 || strings.HasPrefix(got[0], "✅") {
		t.Fatalf("refresh statuses = %q, want one refresh without an outcome", got)
	}
}

func TestImportMapped_Partial(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	e.sonarr.mu.Lock()
	e.sonarr.writes = nil
	e.sonarr.mu.Unlock()
	statuses := captureRefresh(s)
	_, err := importMapped(e, s, t, importRows(stuckqueue.MappedFile{Path: pathA, EpisodeIDs: []int{1}}))
	if err != nil {
		t.Fatal(err)
	}
	ws := e.sonarr.writesCopy()
	if len(ws) != 1 {
		t.Fatalf("writes = %+v", ws)
	}
	if files, _ := ws[0].Body["files"].([]any); len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if got := statuses(); len(got) != 1 || strings.HasPrefix(got[0], "✅") {
		t.Fatalf("refresh statuses = %q, want one refresh without an outcome", got)
	}
	if res, _, _ := s.State(); len(
		app(res, stuckqueue.AppSonarr).Items,
	)+len(
		app(res, stuckqueue.AppSonarr).Waiting,
	) == 0 {
		t.Fatal("a partial import forgot the download")
	}
}

func TestImportMapped_TargetsChanged(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	_, err := s.ImportMapped(context.Background(), 1, "D", "stale-fingerprint", bothRows())
	refusedNoWrite(t, e.sonarr, err, "this download changed; propose again")
}

func TestImportMapped_Gone(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	if err := seedSeen(e, stuckqueue.AppSonarr, "D", e.clock.Now().Add(-7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := s.ImportMapped(context.Background(), 1, "D", "x", bothRows())
	refusedNoWrite(t, e.sonarr, err, "no longer in the Sonarr queue")
}

func TestImportMapped_NotStuck(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	busy := rawRec(3, "D", "Show", 3, false)
	busy["status"], busy["trackedDownloadState"] = "downloading", "downloading"
	e.sonarr.set(rawRec(1, "D", "Show", 1, false, sample), busy)
	_, err := s.ImportMapped(context.Background(), 1, "D", "x", bothRows())
	refusedNoWrite(t, e.sonarr, err, "no longer stuck")
}

func TestImportMapped_OtherSeries(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	other := rawRec(2, "D", "Show", 2, false, sample)
	other["seriesId"] = float64(9)
	e.sonarr.set(rawRec(1, "D", "Show", 1, false, sample), other)
	_, err := s.ImportMapped(context.Background(), 1, "D", "x", bothRows())
	refusedNoWrite(t, e.sonarr, err, "not only this show's")
}

func TestImportMapped_PendingCommand(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	e.sonarr.setCommands(0, map[string]any{
		"name": "ManualImport", "status": "started",
		"body": map[string]any{"files": []map[string]any{{"path": pathB}}},
	})
	_, err := importMapped(e, s, t, bothRows())
	refusedNoWrite(t, e.sonarr, err, "an import for this download is already running")
}

func TestImportMapped_InvalidRowRefusesAll(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	_, err := importMapped(e, s, t, importRows(
		stuckqueue.MappedFile{Path: pathA, EpisodeIDs: []int{1}},
		stuckqueue.MappedFile{Path: "/elsewhere/x.mkv", EpisodeIDs: []int{2}},
	))
	refusedNoWrite(t, e.sonarr, err, "not a file of this download")
}

func TestImportMapped_Locked(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	_, err := s.ImportMapped(context.Background(), 1, "D", "x", bothRows())
	close(gate)
	s.Wait()
	refusedNoWrite(t, e.sonarr, err, "a stuck-queue run is active, try again in a moment")
}

func TestImportMapped_CommandReadFails(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	e.sonarr.setCommands(500)
	_, err := importMapped(e, s, t, bothRows())
	if err == nil || errors.Is(err, stuckqueue.ErrRefused) {
		t.Fatalf("err = %v, want a plain read error", err)
	}
	if ws := e.sonarr.writesCopy(); len(ws) != 0 {
		t.Fatalf("writes = %+v", ws)
	}
}

func TestImportMapped_NoRows(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	_, err := s.ImportMapped(context.Background(), 1, "D", "x", nil)
	refusedNoWrite(t, e.sonarr, err, "Tick at least one file.")
}
