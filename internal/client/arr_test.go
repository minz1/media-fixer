package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

func TestArr_SearchSeries_NormalizedMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		query string
	}{
		{"exact match", "The Boys"},
		{"lowercase, no article", "boys"},
		{"trailing year", "The Boys (2019)"},
		{"punctuation", "The, Boys!"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v3/series" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode([]client.Series{
					{ID: 7, Title: "The Boys"},
					{ID: 8, Title: "Gen V"},
				})
			}))
			defer srv.Close()

			c := client.NewArr(srv.URL, "key")
			series, err := c.SearchSeries(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("SearchSeries(%q): %v", tc.query, err)
			}
			if series.ID != 7 {
				t.Errorf("SearchSeries(%q) = id %d, want 7", tc.query, series.ID)
			}
		})
	}
}

func TestArr_SearchSeries_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Series{{ID: 1, Title: "Gen V"}})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	_, err := c.SearchSeries(context.Background(), "Completely Unrelated Show")
	if !errors.Is(err, client.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestArr_SearchMovie_NormalizedMatch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/movie" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]client.Movie{
			{ID: 42, Title: "Dune", MovieFileID: 99, HasFile: true},
		})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	movie, err := c.SearchMovie(context.Background(), "dune (2021)")
	if err != nil {
		t.Fatalf("SearchMovie: %v", err)
	}
	if movie.ID != 42 || movie.MovieFileID != 99 {
		t.Errorf("got %+v", movie)
	}
}

// TestArr_PathPrefixedBase_NoDoublePrefix is a regression test for a bug
// where GetEpisodes/GetEpisodeFiles/GetMovieFiles/SeriesGrabHistory/
// MovieGrabHistory built their query string via url.Parse(base+"/api/v3/...")
// but then passed u.RequestURI() (which already includes base's path) back
// into get(), which prepended base a second time. Against a bare-host test
// server (no path component in base) the bug was invisible; it only shows up
// with a path-prefixed base like production's "http://host:8989/sonarr" —
// exactly what this test uses.
func TestArr_PathPrefixedBase_NoDoublePrefix(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/sonarr/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seriesId") != "7" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.Episode{})
	})
	mux.HandleFunc("/sonarr/api/v3/episodefile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.EpisodeFile{})
	})
	mux.HandleFunc("/sonarr/api/v3/history/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{})
	})
	mux.HandleFunc("/radarr/api/v3/moviefile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.MovieFile{})
	})
	mux.HandleFunc("/radarr/api/v3/history/movie", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{})
	})
	// Catches a doubled prefix (e.g. /sonarr/sonarr/...), which would 404 on
	// this mux and previously surfaced as an HTML error page downstream.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sonarr := client.NewArr(srv.URL+"/sonarr", "key")
	if _, err := sonarr.GetEpisodes(context.Background(), 7, -1); err != nil {
		t.Errorf("GetEpisodes: %v", err)
	}
	if _, err := sonarr.GetEpisodeFiles(context.Background(), 7); err != nil {
		t.Errorf("GetEpisodeFiles: %v", err)
	}
	if _, err := sonarr.SeriesGrabHistory(context.Background(), 7, -1); err != nil {
		t.Errorf("SeriesGrabHistory: %v", err)
	}

	radarr := client.NewArr(srv.URL+"/radarr", "key")
	if _, err := radarr.GetMovieFiles(context.Background(), 42); err != nil {
		t.Errorf("GetMovieFiles: %v", err)
	}
	if _, err := radarr.MovieGrabHistory(context.Background(), 42); err != nil {
		t.Errorf("MovieGrabHistory: %v", err)
	}
}

func TestArr_GetEpisodeFiles(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/episodefile" || r.URL.Query().Get("seriesId") != "7" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.EpisodeFile{
			{ID: 100, SeriesID: 7, Path: "/mnt/decypharr/__all__/The.Boys.S01/ep01.mkv", Size: 123},
		})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	files, err := c.GetEpisodeFiles(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != 100 {
		t.Errorf("got %+v", files)
	}
}

func TestArr_DeleteEpisodeFile(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v3/episodefile/100" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	if err := c.DeleteEpisodeFile(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
}

func TestArr_GetMovieFiles(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/moviefile" || r.URL.Query().Get("movieId") != "42" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.MovieFile{{ID: 99, MovieID: 42, Path: "/data/library/movies/Dune.mkv"}})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	files, err := c.GetMovieFiles(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != 99 {
		t.Errorf("got %+v", files)
	}
}

func TestArr_DeleteMovieFile(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v3/moviefile/99" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	if err := c.DeleteMovieFile(context.Background(), 99); err != nil {
		t.Fatal(err)
	}
}

func TestArr_SeriesGrabHistory(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v3/history/series" || q.Get("seriesId") != "7" ||
			q.Get("eventType") != "grabbed" || q.Get("seasonNumber") != "1" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{
			{ID: 555, SeriesID: 7, EventType: "grabbed", SourceTitle: "The.Boys.S01E01"},
		})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	records, err := c.SeriesGrabHistory(context.Background(), 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != 555 {
		t.Errorf("got %+v", records)
	}
}

func TestArr_SeriesGrabHistory_AllSeasons(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("seasonNumber") {
			t.Errorf("expected no seasonNumber param, got %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	if _, err := c.SeriesGrabHistory(context.Background(), 7, -1); err != nil {
		t.Fatal(err)
	}
}

func TestArr_MovieGrabHistory(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v3/history/movie" || q.Get("movieId") != "42" || q.Get("eventType") != "grabbed" {
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{{ID: 321, MovieID: 42, EventType: "grabbed"}})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	records, err := c.MovieGrabHistory(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != 321 {
		t.Errorf("got %+v", records)
	}
}

func TestArr_MarkHistoryFailed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/history/failed/555" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	if err := c.MarkHistoryFailed(context.Background(), 555); err != nil {
		t.Fatal(err)
	}
}

func TestArr_SeasonSearch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "SeasonSearch" || body["seriesId"] != float64(7) || body["seasonNumber"] != float64(1) {
			t.Errorf("unexpected body: %v", body)
		}
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	if err := c.SeasonSearch(context.Background(), 7, 1); err != nil {
		t.Fatal(err)
	}
}

func TestArr_SeriesSearch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "SeriesSearch" || body["seriesId"] != float64(7) {
			t.Errorf("unexpected body: %v", body)
		}
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	if err := c.SeriesSearch(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
}

func TestArr_SystemStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/system/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "key" {
			t.Errorf("missing api key header")
		}
		_ = json.NewEncoder(w).Encode(client.SystemStatus{Version: "4.0.0"})
	}))
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	status, err := c.SystemStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "4.0.0" {
		t.Errorf("got %+v", status)
	}
}
