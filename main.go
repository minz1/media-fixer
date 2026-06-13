package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/config"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/server"
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

type agentBundle struct {
	ag      *agent.Agent
	summary *agent.Summarizer
	ctrl    *agent.ControlReviewer
}

func buildAgentComponents(cfg *config.Config, database *db.DB, log *slog.Logger) (*agentBundle, error) {
	decypharr := client.NewDecypharr(cfg.Decypharr.URL, cfg.Decypharr.APIToken)
	jellyfin := client.NewJellyfin(cfg.Jellyfin.URL, cfg.Jellyfin.APIKey)
	sonarr := client.NewArr(cfg.Sonarr.URL, cfg.Sonarr.APIKey)
	radarr := client.NewArr(cfg.Radarr.URL, cfg.Radarr.APIKey)

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

	llmCfg := openai.DefaultConfig(cfg.LLM.APIKey)
	if cfg.LLM.BaseURL != "" {
		llmCfg.BaseURL = cfg.LLM.BaseURL
	}
	llmClient := openai.NewClientWithConfig(llmCfg)

	disp := &agent.Dispatcher{
		Decypharr:  decypharr,
		Jellyfin:   jellyfin,
		Sonarr:     sonarr,
		Radarr:     radarr,
		Loki:       loki,
		MediaAgent: mediaAgent,
		DB:         database,
	}

	b := &agentBundle{
		ag:      agent.New(llmClient, cfg.LLM.Model, disp, database, log),
		summary: agent.NewSummarizer(llmClient, cfg.LLM.Model),
	}

	if cfg.ControlLLM != nil {
		controlCfg := openai.DefaultConfig(cfg.ControlLLM.APIKey)
		if cfg.ControlLLM.BaseURL != "" {
			controlCfg.BaseURL = cfg.ControlLLM.BaseURL
		}
		b.ctrl = agent.NewControlReviewer(openai.NewClientWithConfig(controlCfg), cfg.ControlLLM.Model, log)
	}

	return b, nil
}

func run() error {
	cfgPath := flag.String("config", "/etc/media-fixer/config.toml", "path to TOML config file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "error", err)
		return err
	}

	database, err := db.Open(cfg.DB.Path)
	if err != nil {
		log.Error("open database", "error", err)
		return err
	}
	defer database.Close()

	bundle, err := buildAgentComponents(cfg, database, log)
	if err != nil {
		log.Error("build agent components", "error", err)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bot, err := discord.New(cfg.Discord.Token, cfg.Discord.GuildID, cfg.Discord.OwnerID, log)
	if err != nil {
		log.Error("init discord bot", "error", err)
		return err
	}

	svc := incident.NewService(database, bundle.ag, bundle.ctrl, bundle.summary, bot, log)
	bot.SetService(svc)

	if err = bot.Start(); err != nil {
		log.Error("start discord bot", "error", err)
		return err
	}
	defer bot.Close()

	srv, err := server.New(cfg.Server.Addr, cfg.Server.BaseURL, database, svc, log)
	if err != nil {
		log.Error("init server", "error", err)
		return err
	}

	go svc.RecoverZombies(context.WithoutCancel(ctx))

	log.Info("media-fixer started")
	if err = srv.Start(ctx); err != nil {
		log.Error("server stopped", "error", err)
		return err
	}
	return nil
}
