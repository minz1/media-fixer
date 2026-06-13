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
	item, err := c.SearchItem(context.Background(), "Breaking Bad")
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "item-1" {
		t.Errorf("id: %q", item.ID)
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
