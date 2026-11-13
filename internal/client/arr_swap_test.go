package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

func TestD3_FolderPreviewNeverSendsSeriesID(t *testing.T) {
	t.Parallel()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got = append(got, r.URL.RawQuery)
		if q.Has("seriesId") {
			_, _ = w.Write([]byte(`[{"path":"/library/old.mkv","size":1}]`))
			return
		}
		_, _ = w.Write(
			[]byte(`[{"path":"/data/downloads/sonarr/R/r.mkv","size":42,"customFormatScore":7,"series":{"id":3}}]`),
		)
	}))
	t.Cleanup(srv.Close)
	cs, err := client.NewArr(srv.URL, "key").
		ManualImportFolderCandidates(context.Background(), "/data/downloads/sonarr/R")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "filterExistingFiles=false&folder=%2Fdata%2Fdownloads%2Fsonarr%2FR" {
		t.Fatalf("query = %v", got)
	}
	if len(cs) != 1 || cs[0].Path != "/data/downloads/sonarr/R/r.mkv" || cs[0].CustomFormatScore != 7 ||
		cs[0].SeriesID != 3 {
		t.Fatalf("candidates = %+v", cs)
	}
}

const swapHistory = `[
{"id":1,"episodeId":9,"eventType":"grabbed","sourceTitle":"Overgeared.S01E06.VARYG","downloadId":"VARYG","date":"2026-10-01T00:00:00Z"},
{"id":2,"episodeId":9,"eventType":"downloadFolderImported","downloadId":"VARYG","date":"2026-10-01T01:00:00Z","data":{"importedPath":"/tv/Overgeared/S01E06 VARYG.mkv"}},
{"id":3,"episodeId":9,"eventType":"grabbed","sourceTitle":"Overgeared.S01E06.AnoZu","downloadId":"ANOZU","date":"2026-10-05T00:00:00Z"},
{"id":4,"episodeId":9,"eventType":"downloadFolderImported","downloadId":"ANOZU","date":"2026-10-05T01:00:00Z","data":{"importedPath":"/tv/Overgeared/S01E06 AnoZu.mkv"}},
{"id":5,"episodeId":9,"eventType":"grabbed","sourceTitle":"Overgeared.S01E06.BiOMA","downloadId":"BIOMA","date":"2026-10-09T00:00:00Z"},
{"id":6,"episodeId":8,"eventType":"grabbed","sourceTitle":"Overgeared.S01E05.AnoZu","downloadId":"OTHER","date":"2026-10-06T00:00:00Z"}
]`

func grabFor(t *testing.T, body, current string) client.SwapGrab {
	t.Helper()
	c, _ := captureServer(t, body)
	g, err := c.GrabBehindFile(context.Background(), 7, 9, current)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestD3_GrabBehindFile(t *testing.T) {
	t.Parallel()
	g := grabFor(t, swapHistory, "/library/Overgeared/S01E06 AnoZu.mkv")
	if g.SkipBlocklist || g.HistoryID != 3 || g.DownloadID != "ANOZU" || g.SourceTitle != "Overgeared.S01E06.AnoZu" ||
		!slices.Equal(g.EpisodeIDs, []int{9}) || g.GrabbedAt.IsZero() {
		t.Fatalf("grab = %+v", g)
	}
	if own := g.WithCandidate("anozu"); !own.SkipBlocklist || own.Note == "" {
		t.Fatalf("candidate's own grab = %+v", own)
	}
	if other := g.WithCandidate("BIOMA"); other.SkipBlocklist {
		t.Fatalf("other candidate = %+v", other)
	}

	unknown := grabFor(t, swapHistory, "/library/Overgeared/S01E06 BiOMA.mkv")
	if !unknown.SkipBlocklist || unknown.DownloadID != "" || unknown.HistoryID != 0 ||
		unknown.Note != "the current file's grab is unknown; blocklist skipped" {
		t.Fatalf("mismatch = %+v", unknown)
	}

	noPath := grabFor(
		t,
		`[{"id":2,"episodeId":9,"eventType":"downloadFolderImported","downloadId":"X","date":"2026-10-01T01:00:00Z","data":{"fileId":1,"droppedPath":"/d/a.mkv"}}]`,
		"/library/a.mkv",
	)
	if !noPath.SkipBlocklist || noPath.DownloadID != "" {
		t.Fatalf("no importedPath = %+v", noPath)
	}

	empty := grabFor(t, `[
{"id":1,"episodeId":9,"eventType":"grabbed","sourceTitle":"x","downloadId":"","date":"2026-10-01T00:00:00Z"},
{"id":2,"episodeId":9,"eventType":"downloadFolderImported","downloadId":"","date":"2026-10-01T01:00:00Z","data":{"importedPath":"/tv/a.mkv"}}]`, "/library/a.mkv")
	if !empty.SkipBlocklist || empty.DownloadID != "" || empty.HistoryID != 0 {
		t.Fatalf("empty id = %+v", empty)
	}

	pack := grabFor(t, `[
{"id":1,"episodeId":9,"eventType":"grabbed","sourceTitle":"Pack","downloadId":"PACK","date":"2026-10-01T00:00:00Z"},
{"id":2,"episodeId":10,"eventType":"grabbed","sourceTitle":"Pack","downloadId":"PACK","date":"2026-10-01T00:00:00Z"},
{"id":3,"episodeId":9,"eventType":"downloadFolderImported","downloadId":"PACK","date":"2026-10-01T01:00:00Z","data":{"importedPath":"/tv/a.mkv"}}]`, "/library/a.mkv")
	if !pack.SkipBlocklist || pack.DownloadID != "PACK" || !slices.Equal(pack.EpisodeIDs, []int{9, 10}) ||
		pack.Note != "pack grab: blocklist skipped" {
		t.Fatalf("pack = %+v", pack)
	}
}

func TestD3_RecycleBin(t *testing.T) {
	t.Parallel()
	c, calls := captureServer(t, `{"recycleBin":"/data/recycle","autoUnmonitorPreviouslyDownloadedEpisodes":false}`)
	got, err := c.RecycleBin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "/data/recycle" || (*calls)[0].path != "/api/v3/config/mediamanagement" ||
		(*calls)[0].method != http.MethodGet {
		t.Fatalf("bin = %q, calls = %+v", got, *calls)
	}
}
