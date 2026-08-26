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
