package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/config"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/server"
	"github.com/rs/zerolog"
	openai "github.com/sashabaranov/go-openai"
)

func main() {
	cfgPath := flag.String("config", "/etc/media-fixer/config.toml", "path to TOML config file")
	flag.Parse()

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal().Err(err).Msg("load config")
	}

	database, err := db.Open(cfg.DB.Path)
	if err != nil {
		log.Fatal().Err(err).Msg("open database")
	}
	defer database.Close()

	// Build clients.
	decypharrClient := client.NewDecypharr(cfg.Decypharr.URL, cfg.Decypharr.APIToken)
	jellyfinClient := client.NewJellyfin(cfg.Jellyfin.URL, cfg.Jellyfin.APIKey)
	sonarrClient := client.NewArr(cfg.Sonarr.URL, cfg.Sonarr.APIKey)
	radarrClient := client.NewArr(cfg.Radarr.URL, cfg.Radarr.APIKey)
	lokiClient := client.NewLoki(cfg.Loki.URL)

	var mediaClient *client.MediaHostClient
	if cfg.Media.Host != "" && cfg.Media.SSHKeyPath != "" {
		mediaClient, err = client.NewMediaHost(
			cfg.Media.Host, cfg.Media.Port, cfg.Media.User, cfg.Media.SSHKeyPath,
		)
		if err != nil {
			log.Fatal().Err(err).Msg("init media host client")
		}
	} else {
		log.Warn().Msg("media host not configured — dd tests and remote restarts unavailable")
	}

	// LLM client (OpenAI-compatible).
	llmCfg := openai.DefaultConfig(cfg.LLM.APIKey)
	if cfg.LLM.BaseURL != "" {
		llmCfg.BaseURL = cfg.LLM.BaseURL
	}
	llmClient := openai.NewClientWithConfig(llmCfg)

	// Wire the agent dispatcher.
	disp := &agent.Dispatcher{
		Decypharr: decypharrClient,
		Jellyfin:  jellyfinClient,
		Sonarr:    sonarrClient,
		Radarr:    radarrClient,
		Loki:      lokiClient,
		Media:     mediaClient,
		DB:        database,
	}

	ag := agent.New(llmClient, cfg.LLM.Model, disp, database, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bootstrap: incident service and Discord bot have a mutual dependency
	// (service needs the bot as a Notifier; bot needs the service to handle
	// /report commands). Break the cycle by giving the bot a pointer to the
	// service that gets filled in after both are constructed.
	bot, err := discord.New(cfg.Discord.Token, cfg.Discord.GuildID, cfg.Discord.OwnerID, log)
	if err != nil {
		log.Fatal().Err(err).Msg("init discord bot")
	}

	svc := incident.NewService(database, ag, bot, log)
	bot.SetService(svc)

	if err := bot.Start(); err != nil {
		log.Fatal().Err(err).Msg("start discord bot")
	}
	defer bot.Close()

	srv := server.New(cfg.Server.Addr, cfg.Server.BaseURL, database, svc, log)

	log.Info().Msg("media-fixer started")
	if err := srv.Start(ctx); err != nil {
		log.Error().Err(err).Msg("server stopped")
	}
}
