package main

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/clientset"
	"github.com/minz1/mediafixer/internal/config"
)

func buildClients(t *testing.T, cfg *config.Config) *clientset.Set {
	t.Helper()
	clients, err := clientset.Build(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return clients
}

func TestD11_3_FeatureSetUnchangedForLiveConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}
	cfg.Decypharr.URL = "http://d"
	cfg.Jellyfin.URL = "http://j"
	cfg.Sonarr.URL = "http://s"
	cfg.Radarr.URL = "http://r"
	cfg.Loki.URL = "http://l"
	cfg.MediaAgent.URL = "http://m"
	cfg.Seerr.URL = "http://se"
	want := []string{"sweep", "stuck-queue:sonarr", "stuck-queue:radarr", "stuck-read", "seerr"}
	if got := enabledFeatures(buildClients(t, cfg)); !slices.Equal(got, want) {
		t.Errorf("enabledFeatures = %v, want %v", got, want)
	}
}

func TestD11_3_NoFeaturesWithoutURLs(t *testing.T) {
	t.Parallel()
	if got := enabledFeatures(buildClients(t, &config.Config{})); len(got) != 0 {
		t.Errorf("enabledFeatures = %v, want none", got)
	}
}
