package livecheck_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestCheckListDirectory_TriesAllCandidateDirs is a regression test: real
// torrent files often live under /mnt/decypharr/__all__/<torrent>/ rather
// than /mnt/decypharr/<torrent> directly (the same layout discoverSamplePath
// already tries both for) — a version of this check that only tried the
// first form ENOENT'd even when the file was there under __all__.
func TestCheckListDirectory_TriesAllCandidateDirs(t *testing.T) {
	t.Parallel()
	const torrent = "The.Boys.S01"
	wantDirs := livecheck.DecypharrCandidateDirsForTest(torrent)
	if len(wantDirs) < 2 {
		t.Fatalf("expected multiple candidate dirs, got %v", wantDirs)
	}
	// Only the last candidate (the __all__ layout) actually exists.
	onlyRealDir := wantDirs[len(wantDirs)-1]

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
	fx := livecheck.Fixtures{TorrentName: torrent}

	run := findCheck(t, "list_directory")
	result := run(context.Background(), disp, &fx, livecheck.Options{})

	if result.Status != livecheck.StatusOK {
		t.Errorf("status = %s, detail = %s, err = %s", result.Status, result.Detail, result.Error)
	}
}
