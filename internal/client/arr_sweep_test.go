package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
)

type sweepCall struct {
	method, path, query, body string
}

func sweepServer(t *testing.T, status int, reply string) (*client.ArrClient, *[]sweepCall) {
	t.Helper()
	var calls []sweepCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, sweepCall{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		if r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("missing api key on %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return client.NewArr(srv.URL+"/sonarr", "key"), &calls
}

func wantCall(t *testing.T, calls []sweepCall, want sweepCall) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want exactly one", calls)
	}
	got := calls[0]
	if got.method != want.method || got.path != want.path || got.query != want.query {
		t.Errorf("call = %s %s?%s, want %s %s?%s", got.method, got.path, got.query, want.method, want.path, want.query)
	}
	if want.body != "" && !jsonEqual(got.body, want.body) {
		t.Errorf("body = %s, want %s", got.body, want.body)
	}
}

func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

const releaseJSON = `[{"guid":"g","indexerId":4,"title":"Show S02 1080p","indexer":"Zilean","protocol":"torrent",
"fullSeason":true,"rejections":["Existing file on disk is of equal or higher preference"],"releaseWeight":30,
"seasonNumber":2,"size":123,"quality":{"quality":{"name":"WEBDL-1080p"}},"mappedEpisodeInfo":[{"id":5},{"id":6}]}]`

func TestArr_Releases(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusOK, releaseJSON)
	rels, err := c.Releases(context.Background(), 3, 2)
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	wantCall(
		t,
		*calls,
		sweepCall{method: http.MethodGet, path: "/sonarr/api/v3/release", query: "seasonNumber=2&seriesId=3"},
	)
	if len(rels) != 1 {
		t.Fatalf("got %d releases, want 1", len(rels))
	}
	r := rels[0]
	if r.GUID != "g" || r.IndexerID != 4 || r.Indexer != "Zilean" || r.Protocol != "torrent" || !r.FullSeason ||
		r.ReleaseWeight != 30 || r.SeasonNumber != 2 || r.Size != 123 || r.Quality.Quality.Name != "WEBDL-1080p" ||
		len(r.Rejections) != 1 || len(r.MappedEpisodeInfo) != 2 || r.MappedEpisodeInfo[1].ID != 6 {
		t.Errorf("decoded release = %+v", r)
	}
}

func TestArr_EpisodeReleases(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusOK, releaseJSON)
	if _, err := c.EpisodeReleases(context.Background(), 9); err != nil {
		t.Fatalf("EpisodeReleases: %v", err)
	}
	wantCall(t, *calls, sweepCall{method: http.MethodGet, path: "/sonarr/api/v3/release", query: "episodeId=9"})
}

func TestArr_GrabRelease(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusOK, `{}`)
	if err := c.GrabRelease(context.Background(), "g", 4); err != nil {
		t.Fatalf("GrabRelease: %v", err)
	}
	wantCall(
		t,
		*calls,
		sweepCall{method: http.MethodPost, path: "/sonarr/api/v3/release", body: `{"guid":"g","indexerId":4}`},
	)
}

func TestArr_GrabRelease_StatusError(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusInternalServerError, `{"message":"451 torrent_blocked"}`)
	err := c.GrabRelease(context.Background(), "g", 4)
	var se *client.HTTPStatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *HTTPStatusError", err)
	}
	if se.Status != http.StatusInternalServerError || se.Body != `{"message":"451 torrent_blocked"}` {
		t.Errorf("status error = %+v", se)
	}
	if len(*calls) != 1 {
		t.Errorf("got %d POSTs, want 1 (never retried)", len(*calls))
	}
}

func TestArr_GrabRelease_SlowTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c := client.NewArr(srv.URL, "key")
	c.SetSlowTimeout(20 * time.Millisecond)
	if err := c.GrabRelease(context.Background(), "g", 4); err == nil {
		t.Fatal("GrabRelease succeeded, want a client timeout")
	}
}

func TestArr_HistorySince(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(
		t,
		http.StatusOK,
		`[{"id":1,"episodeId":5,"sourceTitle":"Show S02","downloadId":"ABC","eventType":"grabbed","date":"2026-10-22T12:00:00Z"}]`,
	)
	since := time.Date(2026, 10, 22, 11, 55, 0, 0, time.UTC)
	recs, err := c.HistorySince(context.Background(), since)
	if err != nil {
		t.Fatalf("HistorySince: %v", err)
	}
	wantCall(
		t,
		*calls,
		sweepCall{method: http.MethodGet, path: "/sonarr/api/v3/history/since", query: "date=2026-10-22T11%3A55%3A00Z"},
	)
	if len(recs) != 1 || recs[0].DownloadID != "ABC" || !recs[0].Date.Equal(since.Add(5*time.Minute)) {
		t.Errorf("records = %+v", recs)
	}
}

func TestArr_QueueDetails(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusOK, `[{"id":12,"downloadId":"ABC","seriesId":3,"episodeId":5,"seasonNumber":2,
"title":"Show S02","trackedDownloadState":"importPending",
"statusMessages":[{"title":"Show S02E01","messages":["Not an upgrade for existing episode file(s)"]}]}]`)
	qs, err := c.QueueDetails(context.Background())
	if err != nil {
		t.Fatalf("QueueDetails: %v", err)
	}
	wantCall(
		t,
		*calls,
		sweepCall{method: http.MethodGet, path: "/sonarr/api/v3/queue/details", query: "includeEpisode=true"},
	)
	if len(qs) != 1 {
		t.Fatalf("got %d records, want 1", len(qs))
	}
	q := qs[0]
	if q.ID != 12 || q.DownloadID != "ABC" || q.SeriesID != 3 || q.EpisodeID != 5 || q.SeasonNumber != 2 ||
		q.TrackedDownloadState != "importPending" || len(q.StatusMessages) != 1 ||
		q.StatusMessages[0].Messages[0] != "Not an upgrade for existing episode file(s)" {
		t.Errorf("decoded queue detail = %+v", q)
	}
}

func TestArr_RemoveQueueItem(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusOK, ``)
	if err := c.RemoveQueueItem(context.Background(), 12, false, true, true); err != nil {
		t.Fatalf("RemoveQueueItem: %v", err)
	}
	wantCall(t, *calls, sweepCall{method: http.MethodDelete, path: "/sonarr/api/v3/queue/12",
		query: "blocklist=true&removeFromClient=false&skipRedownload=true"})
}

func TestArr_RemoveQueueItem_NotFound(t *testing.T) {
	t.Parallel()
	c, _ := sweepServer(t, http.StatusNotFound, ``)
	err := c.RemoveQueueItem(context.Background(), 12, false, false, false)
	if !errors.Is(err, client.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestArr_SearchEpisodes(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusCreated, `{"id":77,"name":"EpisodeSearch"}`)
	id, err := c.SearchEpisodes(context.Background(), []int{1, 2})
	if err != nil {
		t.Fatalf("SearchEpisodes: %v", err)
	}
	wantCall(
		t,
		*calls,
		sweepCall{
			method: http.MethodPost,
			path:   "/sonarr/api/v3/command",
			body:   `{"name":"EpisodeSearch","episodeIds":[1,2]}`,
		},
	)
	if id != 77 {
		t.Errorf("command id = %d, want 77", id)
	}
}

func TestArr_Command(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(t, http.StatusOK, `{"id":77,"status":"completed"}`)
	status, err := c.Command(context.Background(), 77)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	wantCall(t, *calls, sweepCall{method: http.MethodGet, path: "/sonarr/api/v3/command/77"})
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
}

func TestArr_DownloadClientConfig(t *testing.T) {
	t.Parallel()
	c, calls := sweepServer(
		t,
		http.StatusOK,
		`{"autoRedownloadFailed":true,"autoRedownloadFailedFromInteractiveSearch":true}`,
	)
	cfg, err := c.DownloadClientConfig(context.Background())
	if err != nil {
		t.Fatalf("DownloadClientConfig: %v", err)
	}
	wantCall(t, *calls, sweepCall{method: http.MethodGet, path: "/sonarr/api/v3/config/downloadclient"})
	if !cfg.AutoRedownloadFailed || !cfg.AutoRedownloadFailedFromInteractiveSearch {
		t.Errorf("config = %+v, want both flags on", cfg)
	}
}

func TestArr_SweepGet_StatusError(t *testing.T) {
	t.Parallel()
	c, _ := sweepServer(t, http.StatusServiceUnavailable, `down`)
	_, err := c.QueueDetails(context.Background())
	var se *client.HTTPStatusError
	if !errors.As(err, &se) || se.Status != http.StatusServiceUnavailable {
		t.Errorf("err = %v, want *HTTPStatusError 503", err)
	}
}

func TestArr_SweepFieldsDecoded(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":3,"title":"Show","monitored":true,"seriesType":"anime"}]`)
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(
			w,
			`[{"id":5,"monitored":true,"airDateUtc":"2026-10-01T00:00:00Z","absoluteEpisodeNumber":601}]`,
		)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := client.NewArr(srv.URL, "key")

	series, err := c.ListSeries(context.Background())
	if err != nil {
		t.Fatalf("ListSeries: %v", err)
	}
	if len(series) != 1 || !series[0].Monitored || series[0].SeriesType != "anime" {
		t.Errorf("series = %+v", series)
	}
	eps, err := c.GetEpisodes(context.Background(), 3, -1)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(eps) != 1 || !eps[0].Monitored || eps[0].AbsoluteEpisodeNumber != 601 || eps[0].AirDateUtc == nil ||
		!eps[0].AirDateUtc.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("episodes = %+v", eps)
	}
}
