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

func TestJellyfin_PlaybackInfo_HasSources(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
		}
		if r.Header.Get("X-Emby-Token") != "key" {
			t.Error("missing auth")
		}
		_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{
			MediaSources: []client.MediaSource{{
				ID:                 "src1",
				Path:               "/media/Breaking.Bad.mkv",
				SupportsDirectPlay: true,
			}},
		})
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	result, err := c.PlaybackInfo(context.Background(), "item123")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MediaSources) != 1 {
		t.Fatalf("sources: %d", len(result.MediaSources))
	}
	if result.MediaSources[0].Path != "/media/Breaking.Bad.mkv" {
		t.Errorf("path: %q", result.MediaSources[0].Path)
	}
}

func TestJellyfin_PlaybackInfo_EmptySources(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.PlaybackInfoResult{
			MediaSources: []client.MediaSource{},
			ErrorCode:    "NoCompatibleStream",
		})
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	result, err := c.PlaybackInfo(context.Background(), "item123")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.MediaSources) != 0 {
		t.Error("expected empty sources")
	}
	if result.ErrorCode != "NoCompatibleStream" {
		t.Errorf("error code: %q", result.ErrorCode)
	}
}

func TestJellyfin_SearchItem_Found(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("searchTerm") != "Breaking Bad" {
			t.Errorf("searchTerm: %q", r.URL.Query().Get("searchTerm"))
		}
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{
			Items: []client.JellyfinItem{{
				ID:   "item-1",
				Name: "Breaking Bad",
				Type: "Series",
			}},
			TotalRecordCount: 1,
		})
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	items, err := c.SearchItem(context.Background(), "Breaking Bad")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "item-1" {
		t.Errorf("id: %q", items[0].ID)
	}
}

func TestJellyfin_SearchItem_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{Items: []client.JellyfinItem{}, TotalRecordCount: 0})
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	_, err := c.SearchItem(context.Background(), "Unknown Show")
	if !errors.Is(err, client.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestJellyfin_ListEpisodes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Shows/series-1/Episodes" {
			t.Errorf("path: %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{
			Items: []client.JellyfinItem{
				{ID: "ep-1", Name: "Episode 1", Type: "Episode", Path: "/data/library/tv/Show/s01e01.mkv"},
			},
			TotalRecordCount: 1,
		})
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	eps, err := c.ListEpisodes(context.Background(), "series-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].ID != "ep-1" {
		t.Errorf("episodes: %+v", eps)
	}
}

func TestJellyfin_ListEpisodes_Empty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{Items: []client.JellyfinItem{}, TotalRecordCount: 0})
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	eps, err := c.ListEpisodes(context.Background(), "series-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 0 {
		t.Errorf("expected no episodes, got %d", len(eps))
	}
}

func TestJellyfin_LibraryScan(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Refresh" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	if err := c.LibraryScan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestJellyfin_ScanStatus_Running(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"Key":"SomethingElse","Name":"Other","State":"Idle","CurrentProgressPercentage":0},
			{"Key":"RefreshLibrary","Name":"Scan Media Library","State":"Running","CurrentProgressPercentage":42.5}
		]`))
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	st, err := c.ScanStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Error("expected Running")
	}
	if st.ProgressPct != 42.5 {
		t.Errorf("progress: %v", st.ProgressPct)
	}
}

func TestJellyfin_ScanStatus_Idle(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(
			[]byte(
				`[{"Key":"RefreshLibrary","Name":"Scan Media Library","State":"Idle","CurrentProgressPercentage":0}]`,
			),
		)
	}))
	defer srv.Close()

	c := client.NewJellyfin(srv.URL, "key")
	st, err := c.ScanStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Running {
		t.Error("expected not running")
	}
}
