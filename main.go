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
	"github.com/minz1/mediafixer/internal/clientset"
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
	clients *clientset.Set
}

func buildAgentComponents(cfg *config.Config, database *db.DB, log *slog.Logger) (*agentBundle, error) {
	clients, err := clientset.Build(cfg, log)
	if err != nil {
		return nil, err
	}
	disp := clients.Dispatcher(database)

	llmCfg := openai.DefaultConfig(cfg.LLM.APIKey)
	if cfg.LLM.BaseURL != "" {
		llmCfg.BaseURL = cfg.LLM.BaseURL
	}
	llmClient := openai.NewClientWithConfig(llmCfg)

	b := &agentBundle{
		ag:      agent.New(llmClient, cfg.LLM.Model, disp, database, log),
		summary: agent.NewSummarizer(llmClient, cfg.LLM.Model),
		clients: clients,
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

	svc := incident.NewService(ctx, database, bundle.ag, bundle.ctrl, bundle.summary, bot, log)
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
	// A separate dispatcher with no DB, so live-check runs triggered from the
	// dashboard (which have no real incident to attach action-log rows to)
	// never write actions_log entries — same as the media-fixer-check CLI.
	srv.SetChecker(bundle.clients.Dispatcher(nil))

	go svc.RecoverZombies(context.WithoutCancel(ctx))

	log.Info("media-fixer started")
	if err = srv.Start(ctx); err != nil {
		log.Error("server stopped", "error", err)
		return err
	}
	return nil
}
