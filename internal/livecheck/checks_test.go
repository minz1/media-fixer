package livecheck_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/livecheck"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

// findCheck returns the registered check for tool, failing the test if absent.
func findCheck(
	t *testing.T, tool string,
) func(context.Context, *agent.Dispatcher, *livecheck.Fixtures, livecheck.Options) livecheck.Result {
	t.Helper()
	for _, spec := range livecheck.CheckRegistryForTest() {
		if spec.Tool == tool {
			return spec.Run
		}
	}
	t.Fatalf("no check registered for %q", tool)
	return nil
}

func TestCheckListDirectory_TriesAllCandidateDirs(t *testing.T) {
	t.Parallel()
	const (
		torrent = "[Group] The Boys S01"
		folder  = "The.Boys.S01"
	)
	wantDirs := livecheck.DecypharrCandidateDirsForTest(folder, torrent)
	if len(wantDirs) < 2 {
		t.Fatalf("expected multiple candidate dirs, got %v", wantDirs)
	}
	// Only the __all__/<original_filename> layout actually exists.
	onlyRealDir := "/mnt/decypharr/__all__/" + folder
	if !slices.Contains(wantDirs, onlyRealDir) {
		t.Fatalf("candidate dirs %v missing %s", wantDirs, onlyRealDir)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ls", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path != onlyRealDir {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(mediaagentapi.ErrorResponse{Error: "no such file or directory"})
			return
		}
		_ = json.NewEncoder(w).Encode(mediaagentapi.ListDirResult{
			Path:    path,
			Entries: []mediaagentapi.ListDirEntry{{Name: "ep01.mkv"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	disp := &agent.Dispatcher{MediaAgent: client.NewMediaAgent(srv.URL, "key")}
	fx := livecheck.Fixtures{TorrentName: torrent, TorrentFolder: folder}

	run := findCheck(t, "list_directory")
	result := run(context.Background(), disp, &fx, livecheck.Options{})

	if result.Status != livecheck.StatusOK {
		t.Errorf("status = %s, detail = %s, err = %s", result.Status, result.Detail, result.Error)
	}
}

func TestCheckArrRemoveAndSearch_PassesArgValidation(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	disp := &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
	fx := livecheck.Fixtures{SeriesTitle: "The Legend of Korra"}

	run := findCheck(t, "arr_remove_and_search")
	result := run(context.Background(), disp, &fx, livecheck.Options{})

	if hits.Load() == 0 {
		t.Errorf("arr never contacted: status = %s, detail = %s, err = %s", result.Status, result.Detail, result.Error)
	}
}

func TestCheckRepairSweep_WaitsForActiveRunToFinish(t *testing.T) {
	t.Parallel()

	var statusCalls, runCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repair/status", func(w http.ResponseWriter, _ *http.Request) {
		if statusCalls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"enabled":true,"active_run":{"status":"running","stage":"probing"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"enabled":true,"active_run":null}`))
	})
	mux.HandleFunc("/api/repair/run", func(w http.ResponseWriter, _ *http.Request) {
		runCalls.Add(1)
		_, _ = w.Write([]byte(`{"run_id":"r1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	disp := &agent.Dispatcher{Decypharr: client.NewDecypharr(srv.URL, "token")}
	run := findCheck(t, "decypharr_repair_sweep")
	result := run(context.Background(), disp, &livecheck.Fixtures{}, livecheck.Options{AllowWrite: true})

	if result.Status != livecheck.StatusOK || runCalls.Load() != 1 {
		t.Errorf("status = %s, runs = %d, detail = %s, err = %s",
			result.Status, runCalls.Load(), result.Detail, result.Error)
	}
}

func TestCheckRepairSweep_CancelledWhileWaiting(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var runCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/repair/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"active_run":{"status":"running","stage":"probing"}}`))
	})
	mux.HandleFunc("/api/repair/run", func(w http.ResponseWriter, _ *http.Request) {
		runCalls.Add(1)
		_, _ = w.Write([]byte(`{"run_id":"r1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	disp := &agent.Dispatcher{Decypharr: client.NewDecypharr(srv.URL, "token")}
	run := findCheck(t, "decypharr_repair_sweep")
	result := run(ctx, disp, &livecheck.Fixtures{}, livecheck.Options{AllowWrite: true})

	if result.Status != livecheck.StatusSkipped || runCalls.Load() != 0 {
		t.Errorf("status = %s, runs = %d, detail = %s", result.Status, runCalls.Load(), result.Detail)
	}
}

const (
	decypharrAll   = "/mnt/decypharr/__all__"
	singleFileName = "Dragon.Ball.Super.Beerus.2026.S01E03.1080p.CR.WEB-DL.AAC2.0.H.264-AnoZu.mkv"
)

type lsStub struct {
	mu        sync.Mutex
	listings  map[string][]mediaagentapi.ListDirEntry
	requested []string
}

func (s *lsStub) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	s.mu.Lock()
	s.requested = append(s.requested, path)
	entries, ok := s.listings[path]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(mediaagentapi.ErrorResponse{
			Error: "open " + path + ": no such file or directory",
		})
		return
	}
	_ = json.NewEncoder(w).Encode(mediaagentapi.ListDirResult{Path: path, Entries: entries})
}

func (s *lsStub) wasRequested(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.requested, path)
}

func discoverSampleAgainst(
	t *testing.T, listings map[string][]mediaagentapi.ListDirEntry, fx livecheck.Fixtures,
) (livecheck.Fixtures, livecheck.Result, *lsStub) {
	t.Helper()
	stub := &lsStub{listings: listings}
	mux := http.NewServeMux()
	mux.HandleFunc("/ls", stub.handle)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	disp := &agent.Dispatcher{MediaAgent: client.NewMediaAgent(srv.URL, "key")}
	livecheck.DiscoverSamplePathForTest(context.Background(), disp, &fx)
	result := findCheck(t, "list_directory")(context.Background(), disp, &fx, livecheck.Options{})
	return fx, result, stub
}

func singleFileListings() map[string][]mediaagentapi.ListDirEntry {
	return map[string][]mediaagentapi.ListDirEntry{
		decypharrAll: {
			{Name: "Some.Season.Pack.S01", IsDir: true},
			{Name: singleFileName, IsDir: false},
			{Name: "Other.Movie.2024.mkv", IsDir: false},
		},
	}
}

func TestDiscoverSamplePath_SingleFileTorrent(t *testing.T) {
	t.Parallel()
	fx, _, _ := discoverSampleAgainst(t, singleFileListings(), livecheck.Fixtures{
		TorrentName: singleFileName, TorrentFolder: singleFileName,
	})
	if want := decypharrAll + "/" + singleFileName; fx.SamplePath != want {
		t.Errorf("SamplePath = %q, want %q (notes %q)", fx.SamplePath, want, fx.Notes)
	}
	if !fx.TorrentIsFile {
		t.Error("TorrentIsFile = false, want true")
	}
}

func TestDiscoverSamplePath_MultiFileTorrent(t *testing.T) {
	t.Parallel()
	const folder = "The.Boys.S01"
	fx, result, stub := discoverSampleAgainst(t, map[string][]mediaagentapi.ListDirEntry{
		decypharrAll + "/" + folder: {{Name: "ep01.mkv", IsDir: false}},
	}, livecheck.Fixtures{TorrentName: folder, TorrentFolder: folder})
	if want := decypharrAll + "/" + folder + "/ep01.mkv"; fx.SamplePath != want {
		t.Errorf("SamplePath = %q, want %q", fx.SamplePath, want)
	}
	if fx.TorrentIsFile {
		t.Error("TorrentIsFile = true, want false")
	}
	if result.Status != livecheck.StatusOK {
		t.Errorf("list_directory status = %s, detail = %s, err = %s", result.Status, result.Detail, result.Error)
	}
	if stub.wasRequested(decypharrAll) {
		t.Errorf("listed %s although the torrent folder listed fine", decypharrAll)
	}
}

func TestDiscoverSamplePath_NeitherFileNorListableFolder(t *testing.T) {
	t.Parallel()
	const name = "Unlistable.Pack.S02"
	cases := map[string][]mediaagentapi.ListDirEntry{
		"folder entry that cannot be listed": {{Name: name, IsDir: true}, {Name: "Other.mkv"}},
		"name absent from __all__":           {{Name: "Other.mkv"}},
	}
	for label, entries := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			fx, _, _ := discoverSampleAgainst(t, map[string][]mediaagentapi.ListDirEntry{
				decypharrAll: entries,
			}, livecheck.Fixtures{TorrentName: name, TorrentFolder: name})
			if fx.SamplePath != "" || fx.TorrentIsFile {
				t.Errorf("SamplePath = %q, TorrentIsFile = %v, want neither", fx.SamplePath, fx.TorrentIsFile)
			}
			if !slices.ContainsFunc(fx.Notes, func(n string) bool {
				return strings.Contains(n, "no readable file found")
			}) {
				t.Errorf("notes %q missing the no-readable-file note", fx.Notes)
			}
		})
	}
}

func TestCheckListDirectory_SingleFileTorrentListsItsFolder(t *testing.T) {
	t.Parallel()
	fx, result, stub := discoverSampleAgainst(t, singleFileListings(), livecheck.Fixtures{
		TorrentName: singleFileName, TorrentFolder: singleFileName,
	})
	if !fx.TorrentIsFile {
		t.Fatalf("TorrentIsFile = false, notes %q", fx.Notes)
	}
	if result.Status != livecheck.StatusOK {
		t.Errorf("list_directory status = %s, detail = %s, err = %s", result.Status, result.Detail, result.Error)
	}
	if !stub.wasRequested(decypharrAll) {
		t.Errorf("list_directory never listed %s", decypharrAll)
	}
}
