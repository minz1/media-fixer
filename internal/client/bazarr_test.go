package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
