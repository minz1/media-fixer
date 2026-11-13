package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
)

func TestC9_BazarrCalls(t *testing.T) {
	t.Parallel()
	type call struct{ method, path, query string }
	var got call
	var body string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("key = %q", r.Header.Get("X-Api-Key"))
		}
		got = call{r.Method, r.URL.Path, r.URL.Query().Encode()}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := client.NewBazarr(srv.URL+"/bazarr/", "key")
	ctx := context.Background()
	ep := client.BazarrTarget{SeriesID: 3, EpisodeID: 7}
	mv := client.BazarrTarget{Movie: true, MovieID: 9}
	expect := func(want call) {
		t.Helper()
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	}
	noErr := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	status = http.StatusNoContent
	noErr(c.Search(ctx, ep, "en", true, false))
	expect(call{"PATCH", "/bazarr/api/episodes/subtitles", "episodeid=7&forced=true&hi=false&language=en&seriesid=3"})
	noErr(c.Search(ctx, mv, "en", false, true))
	expect(call{"PATCH", "/bazarr/api/movies/subtitles", "forced=false&hi=true&language=en&radarrid=9"})

	status = http.StatusOK
	body = `{"data":[{"subtitles":[{"code2":"en","path":"/a.srt","hi":true,"file_size":1234},{"code2":"fr","path":null}]}]}`
	subs, err := c.Subtitles(ctx, ep)
	noErr(err)
	expect(call{"GET", "/bazarr/api/episodes", "episodeid%5B%5D=7"})
	if len(subs) != 2 || subs[0].Code2 != "en" || subs[0].FileSize != 1234 || !subs[0].HI || subs[1].Path != "" {
		t.Errorf("subs %+v", subs)
	}
	_, err = c.Subtitles(ctx, mv)
	noErr(err)
	expect(call{"GET", "/bazarr/api/movies", "radarrid%5B%5D=9"})

	body = `{"data":[]}`
	if _, err = c.Subtitles(ctx, ep); !errors.Is(err, client.ErrBazarrItemUnknown) {
		t.Errorf("empty data err = %v", err)
	}

	body = `[{"provider":"opensubtitles","release_info":["a.b","c"],"score":92.5,"language":"en"}]`
	cands, err := c.Candidates(ctx, ep)
	noErr(err)
	expect(call{"GET", "/bazarr/api/providers/episodes", "episodeid=7"})
	want := client.BazarrCandidate{Provider: "opensubtitles", Release: "a.b, c", Score: 92.5, Language: "en"}
	if len(cands) != 1 || cands[0] != want {
		t.Errorf("cands %+v", cands)
	}
	_, err = c.Candidates(ctx, mv)
	noErr(err)
	expect(call{"GET", "/bazarr/api/providers/movies", "radarrid=9"})

	status = http.StatusInternalServerError
	_, subErr := c.Subtitles(ctx, ep)
	_, candErr := c.Candidates(ctx, ep)
	for _, e := range []error{c.Search(ctx, ep, "en", false, false), subErr, candErr} {
		if e == nil || !strings.Contains(e.Error(), "500") {
			t.Errorf("non-2xx err = %v", e)
		}
	}
}

func TestC9_CandidatesTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := client.NewBazarr(srv.URL, "key")
	if c.HTTPClientForTest().Timeout != 0 {
		t.Fatalf("client timeout = %v", c.HTTPClientForTest().Timeout)
	}
	c.SetTimeoutsForTest(50*time.Millisecond, 2*time.Second)
	if _, err := c.Candidates(context.Background(), client.BazarrTarget{EpisodeID: 1}); err != nil {
		t.Fatalf("candidates slower than the call deadline: %v", err)
	}
	if _, err := c.Subtitles(context.Background(), client.BazarrTarget{EpisodeID: 1}); err == nil {
		t.Fatal("subtitles should hit the shortened call deadline")
	}
}

func TestRS1_SyncRequest(t *testing.T) {
	t.Parallel()
	var method, path string
	var q url.Values
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("key = %q", r.Header.Get("X-Api-Key"))
		}
		method, path, q = r.Method, r.URL.Path, r.URL.Query()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := client.NewBazarr(srv.URL, "key")
	ctx := context.Background()

	if err := c.Sync(ctx, client.BazarrTarget{SeriesID: 3, EpisodeID: 7}, "/tv/a.en.srt", "en", true); err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"action": {"sync"}, "language": {"en"}, "path": {"/tv/a.en.srt"}, "type": {"episode"}, "id": {"7"},
		"forced": {"false"}, "hi": {"true"},
	}
	if method != http.MethodPatch || path != "/api/subtitles" || q.Encode() != want.Encode() {
		t.Errorf("episode sync = %s %s %s", method, path, q.Encode())
	}
	if err := c.Sync(ctx, client.BazarrTarget{Movie: true, MovieID: 9}, "/m/b.en.srt", "en", false); err != nil {
		t.Fatal(err)
	}
	want = url.Values{
		"action": {"sync"}, "language": {"en"}, "path": {"/m/b.en.srt"}, "type": {"movie"}, "id": {"9"},
		"forced": {"false"}, "hi": {"false"},
	}
	if q.Encode() != want.Encode() {
		t.Errorf("movie sync query = %s", q.Encode())
	}
	for _, s := range []int{http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError} {
		status = s
		err := c.Sync(ctx, client.BazarrTarget{EpisodeID: 7}, "/tv/a.en.srt", "en", false)
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(s)) {
			t.Errorf("status %d err = %v", s, err)
		}
	}
}

func TestRS1_SyncHistory(t *testing.T) {
	t.Parallel()
	var path string
	var q url.Values
	body := `{"data":[
		{"action":5,"subtitles_path":"/tv/a.en.srt","description":"English subtitles synchronization ended with an offset of 2.31 seconds and a framerate scale factor of 1."},
		{"action":1,"subtitles_path":"/tv/a.en.srt","description":"downloaded"},
		{"action":5,"subtitles_path":"/tv/other.en.srt","description":"other"},
		{"action":5,"subtitles_path":"/tv/a.en.srt","description":"older"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, q = r.URL.Path, r.URL.Query()
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := client.NewBazarr(srv.URL, "key")
	ctx := context.Background()

	n, latest, err := c.SyncHistory(ctx, client.BazarrTarget{SeriesID: 3, EpisodeID: 7}, "/tv/a.en.srt")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/episodes/history" || q.Encode() != "episodeid=7&length=-1" {
		t.Errorf("episode history = %s %s", path, q.Encode())
	}
	if n != 2 || !strings.HasPrefix(latest, "English subtitles synchronization ended") {
		t.Errorf("count %d latest %q", n, latest)
	}
	if _, _, err = c.SyncHistory(ctx, client.BazarrTarget{Movie: true, MovieID: 9}, "/m/b.srt"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/movies/history" || q.Encode() != "length=-1&radarrid=9" {
		t.Errorf("movie history = %s %s", path, q.Encode())
	}
	body = `{"data":[]}`
	n, latest, err = c.SyncHistory(ctx, client.BazarrTarget{EpisodeID: 7}, "/tv/a.en.srt")
	if err != nil || n != 0 || latest != "" {
		t.Errorf("empty history = %d %q %v", n, latest, err)
	}
}
