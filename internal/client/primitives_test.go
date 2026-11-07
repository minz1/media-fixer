package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
)

func TestArr_DeleteEpisodeFile_404IsNotFound(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusNotFound, `{"message":"NotFound"}`)
	if err := c.DeleteEpisodeFile(context.Background(), 9); !errors.Is(err, client.ErrNotFound) {
		t.Errorf("DeleteEpisodeFile err = %v, want ErrNotFound", err)
	}
	if err := c.DeleteMovieFile(context.Background(), 9); !errors.Is(err, client.ErrNotFound) {
		t.Errorf("DeleteMovieFile err = %v, want ErrNotFound", err)
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %+v, want 2", *calls)
	}
}

func TestArr_FailedForDownload_MatchesDownloadIDAfterGrab(t *testing.T) {
	t.Parallel()
	grabbed := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	reply := func(recs ...client.HistoryRecord) string {
		b, _ := json.Marshal(recs)
		return string(b)
	}
	cases := []struct {
		name      string
		mediaType string
		path      string
		query     string
		reply     string
		want      bool
	}{
		{
			"tv match after grab", client.ReplaceMediaTV, "/sonarr/api/v3/history/series",
			"eventType=downloadFailed&seriesId=4",
			reply(client.HistoryRecord{DownloadID: "ABC", Date: grabbed.Add(time.Minute)}), true,
		},
		{
			"movie match at grab time", client.ReplaceMediaMovie, "/sonarr/api/v3/history/movie",
			"eventType=downloadFailed&movieId=4",
			reply(client.HistoryRecord{DownloadID: "ABC", Date: grabbed}), true,
		},
		{
			"failure before the grab", client.ReplaceMediaTV, "/sonarr/api/v3/history/series",
			"eventType=downloadFailed&seriesId=4",
			reply(client.HistoryRecord{DownloadID: "ABC", Date: grabbed.Add(-time.Minute)}), false,
		},
		{
			"other download id", client.ReplaceMediaTV, "/sonarr/api/v3/history/series",
			"eventType=downloadFailed&seriesId=4",
			reply(client.HistoryRecord{DownloadID: "ABCD", Date: grabbed.Add(time.Minute)}), false,
		},
		{"no records", client.ReplaceMediaTV, "/sonarr/api/v3/history/series",
			"eventType=downloadFailed&seriesId=4", `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, calls := sweepServer(t, http.StatusOK, tc.reply)
			got, err := c.FailedForDownload(context.Background(), tc.mediaType, 4, "ABC", grabbed)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("FailedForDownload = %v, want %v", got, tc.want)
			}
			wantCall(t, *calls, sweepCall{method: http.MethodGet, path: tc.path, query: tc.query})
		})
	}
}

func TestArr_FailedForDownload_Errors(t *testing.T) {
	t.Parallel()
	c, _ := sweepServer(t, http.StatusInternalServerError, `boom`)
	if _, err := c.FailedForDownload(context.Background(), client.ReplaceMediaTV, 4, "ABC", time.Now()); err == nil {
		t.Error("a 500 must surface as an error")
	}
	if _, err := c.FailedForDownload(context.Background(), "book", 4, "ABC", time.Now()); err == nil {
		t.Error("an unknown media type must be an error")
	}
}

func TestArr_Search_Commands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		media   int
		season  int
		ids     []int
		body    string
		wantErr bool
	}{
		{"movie", "MoviesSearch", 7, -1, nil, `{"name":"MoviesSearch","movieIds":[7]}`, false},
		{"episode", "EpisodeSearch", 3, -1, []int{11, 12}, `{"name":"EpisodeSearch","episodeIds":[11,12]}`, false},
		{"season", "SeasonSearch", 3, 2, nil, `{"name":"SeasonSearch","seriesId":3,"seasonNumber":2}`, false},
		{"series", "SeriesSearch", 3, -1, nil, `{"name":"SeriesSearch","seriesId":3}`, false},
		{"unknown", "RescanSeries", 3, -1, nil, ``, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, calls := sweepServer(t, http.StatusCreated, `{"id":5}`)
			err := c.Search(context.Background(), tc.command, tc.media, tc.season, tc.ids)
			if tc.wantErr {
				if err == nil || len(*calls) != 0 {
					t.Errorf("err = %v, calls = %+v, want an error and no call", err, *calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantCall(t, *calls, sweepCall{method: http.MethodPost, path: "/sonarr/api/v3/command", body: tc.body})
		})
	}
}

func statusServer(t *testing.T, status int, body string) *client.DecypharrClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return client.NewDecypharr(srv.URL, "")
}

func TestDecypharr_DeleteTorrent_StatusErrorWithBody(t *testing.T) {
	t.Parallel()
	c := statusServer(t, http.StatusNotFound, "no such torrent")
	err := c.DeleteTorrent(context.Background(), "movies", "dead", true)
	se, ok := errors.AsType[*client.HTTPStatusError](err)
	if !ok || se.Status != http.StatusNotFound || se.Body != "no such torrent" {
		t.Fatalf("err = %v, want *HTTPStatusError 404 with the body", err)
	}
	if !strings.Contains(err.Error(), "no such torrent") {
		t.Errorf("message %q lacks the body", err)
	}
}

func TestDecypharr_AddTorrent_StatusErrorWithBody(t *testing.T) {
	t.Parallel()
	c := statusServer(t, http.StatusBadRequest, "bad magnet")
	err := c.AddTorrent(context.Background(), "magnet:?xt=urn:btih:abc", "tv")
	se, ok := errors.AsType[*client.HTTPStatusError](err)
	if !ok || se.Status != http.StatusBadRequest || se.Body != "bad magnet" {
		t.Fatalf("err = %v, want *HTTPStatusError 400 with the body", err)
	}
	if !strings.Contains(err.Error(), "bad magnet") {
		t.Errorf("message %q lacks the body", err)
	}
}

func torrentPager(
	t *testing.T,
	pages map[int][]*client.TorrentEntry,
	total int,
	seen *[]string,
) *client.DecypharrClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		*seen = append(*seen, q.Get("search")+"|"+q.Get("page"))
		_ = json.NewEncoder(w).Encode(client.TorrentListResponse{Torrents: pages[page], Total: total})
	}))
	t.Cleanup(srv.Close)
	return client.NewDecypharr(srv.URL, "")
}

func TestDecypharr_TorrentByHash_ExactHash(t *testing.T) {
	t.Parallel()
	var seen []string
	c := torrentPager(t, map[int][]*client.TorrentEntry{
		1: {{Name: "Show.S01", InfoHash: "abcd"}, {Name: "Show.S01", InfoHash: "ABC"}},
	}, 2, &seen)
	got, err := c.TorrentByHash(context.Background(), "Show.S01", "abc")
	if err != nil || !got {
		t.Fatalf("TorrentByHash(abc) = %v, %v; want true (case-insensitive exact)", got, err)
	}
	got, err = c.TorrentByHash(context.Background(), "Show.S01", "ab")
	if err != nil || got {
		t.Fatalf("TorrentByHash(ab) = %v, %v; a prefix must not match", got, err)
	}
	got, err = c.TorrentByHash(context.Background(), "Show.S01", "")
	if err != nil || got {
		t.Fatalf("TorrentByHash(empty) = %v, %v; want false", got, err)
	}
}

func hashPage(page, n int) []*client.TorrentEntry {
	out := make([]*client.TorrentEntry, n)
	for i := range out {
		out[i] = &client.TorrentEntry{InfoHash: "h" + strconv.Itoa(page) + "-" + strconv.Itoa(i)}
	}
	return out
}

func TestDecypharr_TorrentByHash_PagesToTheHash(t *testing.T) {
	t.Parallel()
	var seen []string
	c := torrentPager(t, map[int][]*client.TorrentEntry{
		1: hashPage(1, 100), 2: hashPage(2, 100), 3: hashPage(3, 50),
	}, 250, &seen)
	got, err := c.TorrentByHash(context.Background(), "Show", "h2-7")
	if err != nil || !got {
		t.Fatalf("TorrentByHash(h2-7) = %v, %v; want true", got, err)
	}
	if len(seen) != 2 || seen[0] != "Show|1" || seen[1] != "Show|2" {
		t.Errorf("requests = %v, want pages 1 and 2 only", seen)
	}

	seen = nil
	got, err = c.TorrentByHash(context.Background(), "Show", "zz")
	if err != nil || got {
		t.Fatalf("TorrentByHash(zz) = %v, %v; want false", got, err)
	}
	if len(seen) != 3 {
		t.Errorf("requests = %v, want to stop after the last page (3)", seen)
	}
}
