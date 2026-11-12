package livecheck_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/livecheck"
)

func TestG3_LivecheckLokiEmptyIsDegraded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"result": []any{}}})
	}))
	t.Cleanup(srv.Close)
	loki, err := client.NewLoki(srv.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	disp := &agent.Dispatcher{
		Sonarr:    client.NewArr(srv.URL+"/sonarr", "key"),
		Radarr:    client.NewArr(srv.URL+"/radarr", "key"),
		Decypharr: client.NewDecypharr(srv.URL+"/decypharr", "token"),
		Jellyfin:  client.NewJellyfin(srv.URL+"/jellyfin", "key"),
		Loki:      loki,
	}
	report := livecheck.New(disp, livecheck.Options{Only: []string{"loki_query"}}).Run(context.Background())
	for _, r := range report.Results {
		if r.Tool == "loki_query" {
			if r.Status != livecheck.StatusDegraded {
				t.Errorf("status = %v (%s), want degraded", r.Status, r.Detail)
			}
			return
		}
	}
	t.Fatal("loki_query did not run")
}
