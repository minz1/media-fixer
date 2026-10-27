package livecheck_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/livecheck"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

func overrideAgainst(
	t *testing.T, listings map[string][]mediaagentapi.ListDirEntry, fx livecheck.Fixtures,
) (livecheck.Fixtures, livecheck.Result, *lsStub) {
	t.Helper()
	stub := &lsStub{listings: listings}
	mux := http.NewServeMux()
	mux.HandleFunc("/ls", stub.handle)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	disp := &agent.Dispatcher{MediaAgent: client.NewMediaAgent(srv.URL, "key")}
	livecheck.DiscoverTorrentAndSampleForTest(context.Background(), disp, &fx)
	result := findCheck(t, "list_directory")(context.Background(), disp, &fx, livecheck.Options{})
	return fx, result, stub
}

func TestOverride_SingleFileTorrentListsItsFolder(t *testing.T) {
	t.Parallel()
	sample := decypharrAll + "/" + singleFileName
	fx, result, stub := overrideAgainst(t, singleFileListings(), livecheck.Fixtures{
		TorrentName: singleFileName, ReaddTorrentName: "readd", SamplePath: sample,
	})
	if !fx.TorrentIsFile {
		t.Errorf("TorrentIsFile = false, want true (notes %q)", fx.Notes)
	}
	if fx.SamplePath != sample {
		t.Errorf("SamplePath = %q, want the override %q", fx.SamplePath, sample)
	}
	if result.Status != livecheck.StatusOK {
		t.Errorf("list_directory status = %s, detail = %s, err = %s", result.Status, result.Detail, result.Error)
	}
	if !stub.wasRequested(decypharrAll) {
		t.Errorf("list_directory never listed %s", decypharrAll)
	}
}

func TestOverride_MultiFileTorrentListsTheTorrentFolder(t *testing.T) {
	t.Parallel()
	const folder = "The.Boys.S01"
	sample := decypharrAll + "/" + folder + "/ep01.mkv"
	fx, result, stub := overrideAgainst(t, map[string][]mediaagentapi.ListDirEntry{
		decypharrAll:                {{Name: folder, IsDir: true}},
		decypharrAll + "/" + folder: {{Name: "ep01.mkv", IsDir: false}},
	}, livecheck.Fixtures{TorrentName: folder, ReaddTorrentName: "readd", SamplePath: sample})
	if fx.TorrentIsFile {
		t.Error("TorrentIsFile = true, want false for a folder")
	}
	if result.Status != livecheck.StatusOK {
		t.Errorf("list_directory status = %s, detail = %s, err = %s", result.Status, result.Detail, result.Error)
	}
	if !stub.wasRequested(decypharrAll + "/" + folder) {
		t.Errorf("list_directory never listed the torrent folder")
	}
}
