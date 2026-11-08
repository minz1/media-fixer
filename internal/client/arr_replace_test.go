package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
)

// newTVFixtureServer serves one series ("The Boys", id 7) with two episodes
// in season 1, each backed by an episode file and a single grab history
// record, matching the shape planEpisode/Season/SeriesReplace expect.
func newTVFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Series{{ID: 7, Title: "The Boys"}})
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seriesId") != "7" {
			t.Errorf("unexpected episode query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.Episode{
			{ID: 501, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 1, EpisodeFileID: 900, HasFile: true},
			{ID: 502, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 2, EpisodeFileID: 901, HasFile: true},
		})
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("seriesId") != "7" {
			t.Errorf("unexpected episodefile query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.EpisodeFile{
			{ID: 900, SeriesID: 7, Path: "/mnt/decypharr/__all__/Boys.S01/ep01.mkv", Size: 100},
			{ID: 901, SeriesID: 7, Path: "/mnt/decypharr/__all__/Boys.S01/ep02.mkv", Size: 200},
		})
	})
	mux.HandleFunc("/api/v3/history/series", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("seriesId") != "7" || q.Get("eventType") != "grabbed" {
			t.Errorf("unexpected history query: %s", r.URL.RawQuery)
		}
		records := []client.HistoryRecord{
			{ID: 1000, EpisodeID: 501, SeriesID: 7, EventType: "grabbed"},
			{ID: 1001, EpisodeID: 502, SeriesID: 7, EventType: "grabbed"},
		}
		// Both fixture episodes are in season 1, so an unfiltered ("series"
		// scope) query and a season=1 query return the same records here —
		// this fixture doesn't need to distinguish them.
		_ = json.NewEncoder(w).Encode(records)
	})

	return httptest.NewServer(mux)
}

func TestArr_PlanReplace_Episode(t *testing.T) {
	t.Parallel()
	srv := newTVFixtureServer(t)
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	plan, err := c.PlanReplace(context.Background(), client.ReplaceRequest{
		MediaType: client.ReplaceMediaTV,
		Title:     "The Boys",
		Scope:     client.ReplaceScopeEpisode,
		Season:    1,
		Episode:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.MediaID != 7 || plan.EpisodeID != 501 || plan.EpisodeNumber != 1 {
		t.Errorf("got %+v", plan)
	}
	if len(plan.Files) != 1 || plan.Files[0].ID != 900 {
		t.Errorf("files = %+v, want just file 900", plan.Files)
	}
	if len(plan.GrabsToBlocklist) != 1 || plan.GrabsToBlocklist[0].ID != 1000 {
		t.Errorf("grabs = %+v, want just grab 1000", plan.GrabsToBlocklist)
	}
}

func TestArr_PlanReplace_Season(t *testing.T) {
	t.Parallel()
	srv := newTVFixtureServer(t)
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	plan, err := c.PlanReplace(context.Background(), client.ReplaceRequest{
		MediaType: client.ReplaceMediaTV,
		Title:     "The Boys",
		Scope:     client.ReplaceScopeSeason,
		Season:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 2 {
		t.Errorf("files = %+v, want both season files", plan.Files)
	}
	if len(plan.GrabsToBlocklist) != 2 {
		t.Errorf("grabs = %+v, want both season grabs", plan.GrabsToBlocklist)
	}
}

func TestArr_PlanReplace_Series(t *testing.T) {
	t.Parallel()
	srv := newTVFixtureServer(t)
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	plan, err := c.PlanReplace(context.Background(), client.ReplaceRequest{
		MediaType: client.ReplaceMediaTV,
		Title:     "the boys",
		Scope:     client.ReplaceScopeSeries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SeasonNumber != -1 || plan.EpisodeNumber != -1 {
		t.Errorf("series-scope plan should have no season/episode number, got %+v", plan)
	}
	if len(plan.Files) != 2 {
		t.Errorf("files = %+v, want the whole series", plan.Files)
	}
}

func TestArr_PlanReplace_Movie(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Movie{{ID: 42, Title: "Dune", MovieFileID: 99, HasFile: true}})
	})
	mux.HandleFunc("/api/v3/moviefile", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("movieId") != "42" {
			t.Errorf("unexpected moviefile query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.MovieFile{
			{ID: 99, MovieID: 42, Path: "/data/library/movies/Dune.mkv", Size: 500},
		})
	})
	mux.HandleFunc("/api/v3/history/movie", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("movieId") != "42" {
			t.Errorf("unexpected history query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]client.HistoryRecord{{ID: 2000, MovieID: 42, EventType: "grabbed"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	plan, err := c.PlanReplace(context.Background(), client.ReplaceRequest{
		MediaType: client.ReplaceMediaMovie,
		Title:     "Dune (2021)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.MediaID != 42 || len(plan.Files) != 1 || plan.Files[0].ID != 99 {
		t.Errorf("got %+v", plan)
	}
	if len(plan.GrabsToBlocklist) != 1 || plan.GrabsToBlocklist[0].ID != 2000 {
		t.Errorf("got %+v", plan.GrabsToBlocklist)
	}
}

func TestArr_PlanReplace_EpisodeNotFound(t *testing.T) {
	t.Parallel()
	srv := newTVFixtureServer(t)
	defer srv.Close()

	c := client.NewArr(srv.URL, "key")
	_, err := c.PlanReplace(context.Background(), client.ReplaceRequest{
		MediaType: client.ReplaceMediaTV,
		Title:     "The Boys",
		Scope:     client.ReplaceScopeEpisode,
		Season:    1,
		Episode:   99,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent episode")
	}
}
