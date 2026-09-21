package livecheck_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

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

// TestCheckListDirectory_TriesAllCandidateDirs is a regression test for two
// layout facts. Real torrent files live under /mnt/decypharr/__all__/<dir>/
// rather than /mnt/decypharr/<dir> directly, and <dir> is the torrent's
// original_filename, not the name decypharr reports to the *arrs — they
// differ whenever the *arr renamed the grab. A version of this check that
// tried only /mnt/decypharr/<name> ENOENT'd even when the file was there.
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
