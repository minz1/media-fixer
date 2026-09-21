package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
			args: map[string]any{"media_type": "tv", "title": "", "scope": "series", "reason": "wrong_content"},
			why: "an empty title is a substring of every title, so fuzzy matching " +
				"returned the first series in the library and deleted it",
		},
		"missing title": {
			args: map[string]any{"media_type": "tv", "scope": "series", "reason": "wrong_content"},
			why:  "same as an empty title once the absent key decodes to \"\"",
		},
		"uncoercible season with scope=season": {
			args: map[string]any{
				"media_type": "tv", "title": "The Boys", "scope": "season",
				"season": "two", "reason": "wrong_content",
			},
			why: "a value that is present but not a number became the -1 sentinel, and " +
				"GetEpisodes omits seasonNumber entirely when season < 0 — deleting every season",
		},
		"boolean season with scope=season": {
			args: map[string]any{
				"media_type": "tv", "title": "The Boys", "scope": "season",
				"season": true, "reason": "wrong_content",
			},
			why: "same widening via a wrong-typed value",
		},
		"missing season with scope=season": {
			args: map[string]any{
				"media_type": "tv", "title": "The Boys", "scope": "season", "reason": "wrong_content",
			},
			why: "same widening, via an absent key rather than a quoted one",
		},
		"missing reason": {
			args: map[string]any{"media_type": "tv", "title": "The Boys", "scope": "series"},
			why: "reason is the premise the plan gets checked against; without one there is " +
				"nothing to check and nothing recorded for spotting a gamed enum in the logs",
		},
		"unknown reason": {
			args: map[string]any{
				"media_type": "tv", "title": "The Boys", "scope": "series", "reason": "because",
			},
			why: "free text would defeat the point of the enum",
		},
		"missing episode with scope=episode": {
			args: map[string]any{
				"media_type": "tv", "title": "The Boys", "scope": "episode",
				"season": 2.0, "reason": "wrong_content",
			},
			why: "scope=episode with no episode falls through to a wider delete",
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
		"numeric season": {
			"media_type": "tv", "title": "The Boys", "scope": "season",
			"season": 2.0, "reason": "wrong_quality",
		},
		// A quoted number is routine output from the Gemini models this runs
		// against; it must coerce, not silently become the sentinel.
		"quoted season": {
			"media_type": "tv", "title": "The Boys", "scope": "season",
			"season": "2", "reason": "wrong_quality",
		},
		"whole series": {
			"media_type": "tv", "title": "The Boys", "scope": "series", "reason": "wrong_content",
		},
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

// planJSON renders a stored ReplacePlan the way PreviewEscalation persists it.
func planJSON(t *testing.T, plan client.ReplacePlan) []byte {
	t.Helper()
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestExecuteApprovedPlan_ChecksTheStatedPremise is the guard that closes the
// asymmetry with arr_search_missing. That tool verifies its own premise ("the
// file is missing") and refuses when *arr already has the file; this one had
// no premise check at all, so a misdiagnosis could delete a file that was
// fine. "A file exists" cannot be the refusal condition here — that is the
// normal case — so the check is against the reason the agent stated.
func TestExecuteApprovedPlan_ChecksTheStatedPremise(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	result := &agent.DiagnosticResult{EscalateAction: agent.EscalateRemoveAndSearch}
	readable := &client.ReadabilityProbe{
		Checked: true, Probed: 1, ReadableFile: "/data/library/tv/Show/ep.mkv", BytesRead: 104857600,
	}

	t.Run("unreadable claim contradicted by a readable file is refused", func(t *testing.T) {
		t.Parallel()
		// No clients on the dispatcher: the refusal must happen before any
		// delete is attempted, so this must fail on the premise, not on a nil
		// client.
		ag := agent.New(nil, "stub", &agent.Dispatcher{}, nil, nil, slog.New(slog.DiscardHandler))
		_, err := ag.ExecuteApprovedPlan(ctx, result, planJSON(t, client.ReplacePlan{
			MediaType: "tv", Title: "Show", Scope: "episode",
			Reason: client.ReplaceReasonUnreadable, Readability: readable,
		}))
		if err == nil {
			t.Fatal("deleted files whose stated reason was contradicted by reading them")
		}
		if !strings.Contains(err.Error(), "read back fine") {
			t.Errorf("err = %v, want the premise refusal", err)
		}
	})

	t.Run("a non-readability reason is not contradicted by a readable file", func(t *testing.T) {
		t.Parallel()
		ag := agent.New(nil, "stub", &agent.Dispatcher{}, nil, nil, slog.New(slog.DiscardHandler))
		_, err := ag.ExecuteApprovedPlan(ctx, result, planJSON(t, client.ReplacePlan{
			MediaType: "tv", Title: "Show", Scope: "episode",
			Reason: client.ReplaceReasonWrongContent, Readability: readable,
		}))
		// It still fails (no Sonarr configured), but it must get past the
		// premise check to do so — wrong_content says nothing about whether
		// the bytes read.
		if err != nil && strings.Contains(err.Error(), "read back fine") {
			t.Error("wrong_content was refused for a readable file; a successful read says " +
				"nothing about whether the content is the right material")
		}
	})

	t.Run("an unchecked probe never stands in for evidence", func(t *testing.T) {
		t.Parallel()
		ag := agent.New(nil, "stub", &agent.Dispatcher{}, nil, nil, slog.New(slog.DiscardHandler))
		for name, probe := range map[string]*client.ReadabilityProbe{
			"no media-agent": {Checked: false},
			"nothing found":  {Checked: true, Probed: 3},
			"never probed":   nil,
		} {
			_, err := ag.ExecuteApprovedPlan(ctx, result, planJSON(t, client.ReplacePlan{
				MediaType: "tv", Title: "Show", Scope: "episode",
				Reason: client.ReplaceReasonUnreadable, Readability: probe,
			}))
			if err != nil && strings.Contains(err.Error(), "read back fine") {
				t.Errorf("%s: refused on an absent signal; unknown must never read as healthy", name)
			}
		}
	})
}

// TestPlanRemoveAndSearch_ProbesReadabilityAndStopsEarly covers the probe's
// two cost properties. Each dd-test pulls up to 100MiB through the FUSE
// mount, so the probe stops at the first readable file and never tests more
// than maxReadabilityProbes of them.
func TestPlanRemoveAndSearch_ProbesReadabilityAndStopsEarly(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		probed []string
	)
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		probed = append(probed, body.Path)
		n := len(probed)
		mu.Unlock()
		// The first two files are broken; the third reads.
		out := map[string]any{"bytes_read": 0, "error": "input/output error"}
		if n >= 3 {
			out = map[string]any{"bytes_read": 104857600}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer media.Close()

	arr := httptest.NewServer(arrStubWithFiles(t, 20))
	defer arr.Close()

	disp := &agent.Dispatcher{
		Sonarr:     client.NewArr(arr.URL, "key"),
		MediaAgent: client.NewMediaAgent(media.URL, ""),
	}

	raw, err := disp.Call(context.Background(), agent.ToolArrRemoveAndSearchName, map[string]any{
		"media_type": "tv", "title": "The Boys", "scope": "series",
		"reason": client.ReplaceReasonUnreadable,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := raw.(*client.ReplacePlan)
	if !ok {
		t.Fatalf("plan type = %T", raw)
	}

	if plan.Readability == nil || !plan.Readability.Checked {
		t.Fatalf("readability = %+v, want a completed probe", plan.Readability)
	}
	if plan.Readability.ReadableFile == "" {
		t.Error("the third file reads fine but the probe reported nothing readable")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(probed) != 3 {
		t.Errorf("probed %d files, want 3: the probe must stop at the first readable one, "+
			"since each test reads up to 100MiB through the mount", len(probed))
	}
}

// arrStubWithFiles serves a Sonarr library whose series has n episode files,
// so a series-scope plan has more targets than the probe cap.
func arrStubWithFiles(t *testing.T, n int) http.Handler {
	t.Helper()
	episodes := make([]map[string]any, 0, n)
	files := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		episodes = append(episodes, map[string]any{
			"id": i, "seasonNumber": 1, "episodeNumber": i, "hasFile": true, "episodeFileId": i,
		})
		files = append(files, map[string]any{
			"id": i, "path": fmt.Sprintf("/data/library/tv/The Boys/S01E%02d.mkv", i), "size": 1,
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "title": "The Boys"}})
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(episodes)
	})
	mux.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(files)
	})
	mux.HandleFunc("/api/v3/history/series", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	return mux
}
