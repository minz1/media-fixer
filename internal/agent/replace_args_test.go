package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

// arr_remove_and_search is the one tool that deletes media files, and every
// argument it takes comes from LLM output via escalate_params. These pin the
// validation at that boundary: the failure mode is not a wrong answer, it is
// a wrong deletion that the owner already approved on a preview built from
// the same bad values.

// replaceArgsDispatcher serves a two-series Sonarr library so a wrong match
// is observable.
func replaceArgsDispatcher(t *testing.T) *agent.Dispatcher {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "title": "Superbad"},
			{"id": 2, "title": "The Boys"},
		})
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 10, "seasonNumber": 1, "episodeNumber": 1, "hasFile": true},
			{"id": 11, "seasonNumber": 2, "episodeNumber": 1, "hasFile": true},
		})
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 10, "path": "/mnt/s01e01.mkv", "size": 1},
			{"id": 11, "path": "/mnt/s02e01.mkv", "size": 1},
		})
	})
	mux.HandleFunc("/api/v3/history/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &agent.Dispatcher{Sonarr: client.NewArr(srv.URL, "key")}
}

// TestPlanRemoveAndSearch_RejectsUnusableArguments covers the argument shapes
// that used to degrade silently into a wider delete than the owner approved.
func TestPlanRemoveAndSearch_RejectsUnusableArguments(t *testing.T) {
	t.Parallel()
	disp := replaceArgsDispatcher(t)
	ctx := context.Background()

	cases := map[string]struct {
		args map[string]any
		why  string
	}{
		"empty title": {
			args: map[string]any{"media_type": "tv", "title": "", "scope": "series"},
			why: "an empty title is a substring of every title, so fuzzy matching " +
				"returned the first series in the library and deleted it",
		},
		"missing title": {
			args: map[string]any{"media_type": "tv", "scope": "series"},
			why:  "same as an empty title once the absent key decodes to \"\"",
		},
		"uncoercible season with scope=season": {
			args: map[string]any{"media_type": "tv", "title": "The Boys", "scope": "season", "season": "two"},
			why: "a value that is present but not a number became the -1 sentinel, and " +
				"GetEpisodes omits seasonNumber entirely when season < 0 — deleting every season",
		},
		"boolean season with scope=season": {
			args: map[string]any{"media_type": "tv", "title": "The Boys", "scope": "season", "season": true},
			why:  "same widening via a wrong-typed value",
		},
		"missing season with scope=season": {
			args: map[string]any{"media_type": "tv", "title": "The Boys", "scope": "season"},
			why:  "same widening, via an absent key rather than a quoted one",
		},
		"missing episode with scope=episode": {
			args: map[string]any{"media_type": "tv", "title": "The Boys", "scope": "episode", "season": 2.0},
			why:  "scope=episode with no episode falls through to a wider delete",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := disp.Call(ctx, agent.ToolArrRemoveAndSearchName, tc.args)
			if err == nil {
				t.Errorf("accepted %v; %s", tc.args, tc.why)
			}
		})
	}
}

// TestPlanRemoveAndSearch_AcceptsWellFormedArguments is the other half: the
// validation must not reject the shapes the tool is actually for, including a
// quoted number, which is now coerced rather than silently sentinelled.
func TestPlanRemoveAndSearch_AcceptsWellFormedArguments(t *testing.T) {
	t.Parallel()
	disp := replaceArgsDispatcher(t)
	ctx := context.Background()

	for name, args := range map[string]map[string]any{
		"numeric season": {"media_type": "tv", "title": "The Boys", "scope": "season", "season": 2.0},
		// A quoted number is routine output from the Gemini models this runs
		// against; it must coerce, not silently become the sentinel.
		"quoted season": {"media_type": "tv", "title": "The Boys", "scope": "season", "season": "2"},
		"whole series":  {"media_type": "tv", "title": "The Boys", "scope": "series"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := disp.Call(ctx, agent.ToolArrRemoveAndSearchName, args); err != nil {
				t.Errorf("rejected a well-formed plan %v: %v", args, err)
			}
		})
	}
}

// TestSearchSeries_DoesNotGuessOnShortOrAmbiguousTitles pins the fuzzy-match
// guards directly. "up" is a substring of "superbad"; "" is a substring of
// everything.
func TestSearchSeries_DoesNotGuessOnShortOrAmbiguousTitles(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "title": "Superbad"},
			{"id": 2, "title": "Super Bad Cops"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	arr := client.NewArr(srv.URL, "key")
	ctx := context.Background()

	for _, title := range []string{"", "up", "sup", "Super"} {
		if got, err := arr.SearchSeries(ctx, title); err == nil {
			t.Errorf("SearchSeries(%q) matched %q; a title this short or this "+
				"ambiguous must not resolve to a deletion target", title, got.Title)
		}
	}

	// An unambiguous substring still resolves — the guard is about ambiguity
	// and length, not about disabling fuzzy matching.
	got, err := arr.SearchSeries(ctx, "Super Bad Cops")
	if err != nil || got.Title != "Super Bad Cops" {
		t.Errorf("SearchSeries(exact) = %v, %v; want the exact match to still work", got, err)
	}
}

// TestSearchSeries_ErrorMentionsNotFound keeps the caller-visible contract
// stable: a refused fuzzy match reports ErrNotFound, which callers already
// handle, rather than some new error class.
func TestSearchSeries_ErrorMentionsNotFound(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "Superbad"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := client.NewArr(srv.URL, "key").SearchSeries(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want a not-found error", err)
	}
}
