package main

import (
	"context"
	"flag"
	"log/slog"
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
	openai "github.com/sashabaranov/go-openai"
)

func main() {
	cfgPath := flag.String("config", "/etc/media-fixer/config.toml", "path to TOML config file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "error", err)
		os.Exit(1)
	}

	database, err := db.Open(cfg.DB.Path)
	if err != nil {
		log.Error("open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	decypharrClient := client.NewDecypharr(cfg.Decypharr.URL, cfg.Decypharr.APIToken)
	jellyfinClient := client.NewJellyfin(cfg.Jellyfin.URL, cfg.Jellyfin.APIKey)
	sonarrClient := client.NewArr(cfg.Sonarr.URL, cfg.Sonarr.APIKey)
	radarrClient := client.NewArr(cfg.Radarr.URL, cfg.Radarr.APIKey)
	lokiClient, err := client.NewLoki(cfg.Loki.URL, cfg.Loki.TLSCert, cfg.Loki.TLSKey)
	if err != nil {
		log.Error("loki client", "error", err)
		os.Exit(1)
	}

	var mediaAgentClient *client.MediaAgentClient
	if cfg.MediaAgent.URL != "" {
		mediaAgentClient = client.NewMediaAgent(cfg.MediaAgent.URL, cfg.MediaAgent.APIKey)
	} else {
		log.Warn("media-agent not configured — dd tests and remote restarts unavailable")
	}

	llmCfg := openai.DefaultConfig(cfg.LLM.APIKey)
	if cfg.LLM.BaseURL != "" {
		llmCfg.BaseURL = cfg.LLM.BaseURL
	}
	llmClient := openai.NewClientWithConfig(llmCfg)

	disp := &agent.Dispatcher{
		Decypharr:  decypharrClient,
		Jellyfin:   jellyfinClient,
		Sonarr:     sonarrClient,
		Radarr:     radarrClient,
		Loki:       lokiClient,
		MediaAgent: mediaAgentClient,
		DB:         database,
	}

	ag := agent.New(llmClient, cfg.LLM.Model, disp, database, log)

	var controlReviewer *agent.ControlReviewer
	if cfg.ControlLLM != nil {
		controlCfg := openai.DefaultConfig(cfg.ControlLLM.APIKey)
		if cfg.ControlLLM.BaseURL != "" {
			controlCfg.BaseURL = cfg.ControlLLM.BaseURL
		}
		controlReviewer = agent.NewControlReviewer(openai.NewClientWithConfig(controlCfg), cfg.ControlLLM.Model, log)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bot, err := discord.New(cfg.Discord.Token, cfg.Discord.GuildID, cfg.Discord.OwnerID, log)
	if err != nil {
		log.Error("init discord bot", "error", err)
		os.Exit(1)
	}

	svc := incident.NewService(database, ag, controlReviewer, bot, log)
	bot.SetService(svc)

	if err := bot.Start(); err != nil {
		log.Error("start discord bot", "error", err)
		os.Exit(1)
	}
	defer bot.Close()

	srv := server.New(cfg.Server.Addr, cfg.Server.BaseURL, database, svc, log)

	log.Info("media-fixer started")
	if err := srv.Start(ctx); err != nil {
		log.Error("server stopped", "error", err)
	}
}
