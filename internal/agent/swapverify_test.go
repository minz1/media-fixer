package agent_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func swapItemAgent(t *testing.T, fileID int, filePath string, fileSize int64, jfPath string) *agent.Agent {
	t.Helper()
	one, six := 1, 6
	sonarr := http.NewServeMux()
	sonarr.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.Episode{{
			ID: 66, SeriesID: 5, SeasonNumber: 1, EpisodeNumber: 6, EpisodeFileID: fileID, HasFile: fileID != 0,
		}})
	})
	sonarr.HandleFunc("/api/v3/episodefile", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]client.EpisodeFile{{ID: fileID, SeriesID: 5, Path: filePath, Size: fileSize}})
	})
	jf := http.NewServeMux()
	jf.HandleFunc("/Shows/jf-series/Episodes", func(w http.ResponseWriter, _ *http.Request) {
		items := []client.JellyfinItem{}
		if jfPath != "" {
			items = append(items, client.JellyfinItem{
				ID: "jf-new", Type: "Episode", Path: jfPath, IndexNumber: &six, ParentIndexNumber: &one,
			})
		}
		_ = json.NewEncoder(w).Encode(client.ItemsResponse{Items: items})
	})
	sSrv, jSrv := httptest.NewServer(sonarr), httptest.NewServer(jf)
	t.Cleanup(sSrv.Close)
	t.Cleanup(jSrv.Close)
	disp := &agent.Dispatcher{Sonarr: client.NewArr(sSrv.URL, "k"), Jellyfin: client.NewJellyfin(jSrv.URL, "k")}
	return agent.New(nil, "", disp, nil, nil, slog.New(slog.DiscardHandler))
}

func swapTargetFixture() lifecycle.SwapTarget {
	return lifecycle.SwapTarget{
		SeriesItemID: "jf-series", Season: 1, Episode: 6, SonarrSeriesID: 5, EpisodeID: 66, OldFileID: 41,
		CandidateSize: 900,
	}
}

func TestD8_SwapVerifyNotYet(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fileID int
		path   string
		jf     string
	}{
		"jellyfin still on the old name": {42, "/data/tv/Show/new.mkv", "/jf/tv/Show/old.mkv"},
		"jellyfin has no item":           {42, "/data/tv/Show/new.mkv", ""},
		"sonarr still has the old file":  {41, "/data/tv/Show/old.mkv", "/jf/tv/Show/old.mkv"},
	} {
		ag := swapItemAgent(t, tc.fileID, tc.path, 900, tc.jf)
		if id, landed, _ := ag.SwapItem(t.Context(), swapTargetFixture()); landed {
			t.Errorf("%s: landed on %q", name, id)
		}
	}
}

func TestD8_SwapVerifySameName(t *testing.T) {
	t.Parallel()
	ag := swapItemAgent(t, 42, "/data/tv/Show/Show.S01E06.mkv", 900, "/jf/tv/Show/Show.S01E06.mkv")
	id, landed, note := ag.SwapItem(t.Context(), swapTargetFixture())
	if !landed || id != "jf-new" || note != "" {
		t.Fatalf("landed %v id %q note %q", landed, id, note)
	}
}

func TestD8_UpgradeLanded(t *testing.T) {
	t.Parallel()
	ag := swapItemAgent(t, 43, "/data/tv/Show/Show.S01E06.mkv", 1500, "/jf/tv/Show/Show.S01E06.mkv")
	id, landed, note := ag.SwapItem(t.Context(), swapTargetFixture())
	if !landed || id != "jf-new" || !strings.Contains(note, "imported a different file") {
		t.Fatalf("landed %v id %q note %q", landed, id, note)
	}
}
