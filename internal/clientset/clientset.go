// Package clientset builds the set of service clients (decypharr, Jellyfin,
// Sonarr/Radarr, Loki, media-agent) shared by the media-fixer server and the
// media-fixer-check live-check CLI, so both wire up the same clients from the
// same config the same way.
package clientset

import (
	"log/slog"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/config"
	"github.com/minz1/mediafixer/internal/db"
)

// Set holds every service client the agent's tools dispatch to. MediaAgent
// may be nil if [config.MediaAgentConfig.URL] is unset.
type Set struct {
	Decypharr  *client.DecypharrClient
	Jellyfin   *client.JellyfinClient
	Sonarr     *client.ArrClient
	Radarr     *client.ArrClient
	Loki       *client.LokiClient
	MediaAgent *client.MediaAgentClient
}

// Build constructs a Set from config. It only fails if the Loki client's TLS
// material can't be loaded; every other client is dialed lazily on first use.
func Build(cfg *config.Config, log *slog.Logger) (*Set, error) {
	loki, err := client.NewLoki(cfg.Loki.URL, cfg.Loki.TLSCert, cfg.Loki.TLSKey)
	if err != nil {
		return nil, err
	}

	var mediaAgent *client.MediaAgentClient
	if cfg.MediaAgent.URL != "" {
		mediaAgent = client.NewMediaAgent(cfg.MediaAgent.URL, cfg.MediaAgent.APIKey)
	} else {
		log.Warn("media-agent not configured — dd tests and remote restarts unavailable")
	}

	return &Set{
		Decypharr:  client.NewDecypharr(cfg.Decypharr.URL, cfg.Decypharr.APIToken),
		Jellyfin:   client.NewJellyfin(cfg.Jellyfin.URL, cfg.Jellyfin.APIKey),
		Sonarr:     client.NewArr(cfg.Sonarr.URL, cfg.Sonarr.APIKey),
		Radarr:     client.NewArr(cfg.Radarr.URL, cfg.Radarr.APIKey),
		Loki:       loki,
		MediaAgent: mediaAgent,
	}, nil
}

// Dispatcher builds an [agent.Dispatcher] wired to this Set. database may be
// nil (e.g. the live-check CLI has no incident to attach action-log rows to);
// [agent.Dispatcher] tolerates a nil DB.
func (s *Set) Dispatcher(database *db.DB) *agent.Dispatcher {
	return &agent.Dispatcher{
		Decypharr:  s.Decypharr,
		Jellyfin:   s.Jellyfin,
		Sonarr:     s.Sonarr,
		Radarr:     s.Radarr,
		Loki:       s.Loki,
		MediaAgent: s.MediaAgent,
		DB:         database,
	}
}
