package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/clientset"
	"github.com/minz1/mediafixer/internal/config"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/server"
	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/sweep"
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

func buildAgentComponents(
	cfg *config.Config, database *db.DB, jrnl *journal.Journal, log *slog.Logger,
) (*agentBundle, error) {
	clients, err := clientset.Build(cfg, log)
	if err != nil {
		return nil, err
	}
	disp := clients.Dispatcher(database, jrnl)

	llmCfg := openai.DefaultConfig(cfg.LLM.APIKey)
	if cfg.LLM.BaseURL != "" {
		llmCfg.BaseURL = cfg.LLM.BaseURL
	}
	llmClient := openai.NewClientWithConfig(llmCfg)

	b := &agentBundle{
		ag:      agent.New(llmClient, cfg.LLM.Model, disp, database, jrnl, log),
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

func startSweep(
	ctx context.Context, sonarrURL string, sonarr *client.ArrClient, database *db.DB, bot *discord.Bot,
	srv *server.Server, log *slog.Logger,
) *sweep.Scheduler {
	if sonarrURL == "" {
		log.WarnContext(ctx, "sonarr not configured — pack sweep unavailable")
		return nil
	}
	sch := sweep.NewScheduler(ctx, &sweep.Runner{
		Sonarr: sonarr, DB: database, Notify: bot.NotifyOwner,
		Now: time.Now, Sleep: sweep.Sleep, Log: log,
	}, database)
	srv.SetSweep(sch)
	sch.Start()
	return sch
}

func startStuckQueue(
	ctx context.Context, cfg *config.Config, clients *clientset.Set, database *db.DB, bot *discord.Bot,
	srv *server.Server, log *slog.Logger, handlers map[string]card.Handler, renderers map[string]card.Renderer,
) *stuckqueue.Scheduler {
	r := &stuckqueue.Runner{DB: database, Notify: bot.NotifyOwner, Now: time.Now, Log: log}
	if cfg.Sonarr.URL != "" {
		r.Sonarr = clients.Sonarr
	}
	if cfg.Radarr.URL != "" {
		r.Radarr = clients.Radarr
	}
	if r.Sonarr == nil && r.Radarr == nil {
		log.WarnContext(ctx, "sonarr and radarr not configured — stuck-queue sweep unavailable")
		return nil
	}
	sch := stuckqueue.NewScheduler(ctx, r, database)
	sch.Refresh = bot.Refresh
	handlers["sq"], renderers["sq"] = sch.CardHandler(), sch.CardRenderer()
	srv.SetStuckQueue(sch)
	sch.Start()
	return sch
}

func startMaintenance(
	ctx context.Context, stop context.CancelFunc, cfg *config.Config, clients *clientset.Set, database *db.DB,
	bot *discord.Bot, srv *server.Server, log *slog.Logger,
	handlers map[string]card.Handler, renderers map[string]card.Renderer,
) func() {
	sweepSch := startSweep(ctx, cfg.Sonarr.URL, clients.Sonarr, database, bot, srv, log)
	stuckSch := startStuckQueue(ctx, cfg, clients, database, bot, srv, log, handlers, renderers)
	return func() {
		stop()
		if sweepSch != nil {
			sweepSch.Wait()
		}
		if stuckSch != nil {
			stuckSch.Wait()
		}
	}
}

func startCards(
	ctx context.Context, bot *discord.Bot, database *db.DB, jrnl *journal.Journal,
	handlers map[string]card.Handler, renderers map[string]card.Renderer,
) func() {
	ch, unsub := jrnl.SubscribeAll()
	bot.StartCards(ctx, database, ch, handlers, renderers)
	return unsub
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
	jrnl := journal.New(database)

	bundle, err := buildAgentComponents(cfg, database, jrnl, log)
	if err != nil {
		log.Error("build agent components", "error", err)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bot, err := discord.New(cfg.Discord.Token, cfg.Discord.GuildID, cfg.Discord.OwnerID, cfg.Server.PublicURL, log)
	if err != nil {
		log.Error("init discord bot", "error", err)
		return err
	}

	svc := incident.NewService(ctx, database, jrnl, bundle.ag, bundle.ctrl, bundle.summary, bot, log)
	if cfg.Seerr.URL != "" {
		svc.SetSeerr(client.NewSeerr(cfg.Seerr.URL, cfg.Seerr.APIKey))
	} else {
		log.Warn("seerr not configured — seerr reporters get no issue updates")
	}
	bot.SetService(svc)

	bot.Start(ctx)
	defer bot.Close()

	srv, err := server.New(cfg.Server.Addr, cfg.Server.BaseURL, database, jrnl, svc, log)
	if err != nil {
		log.Error("init server", "error", err)
		return err
	}
	// A separate dispatcher with no DB/Journal, so live-check runs triggered
	// from the dashboard (which have no real incident to attach action-log
	// rows to) never write actions_log entries — same as the media-fixer-check
	// CLI.
	srv.SetChecker(bundle.clients.Dispatcher(nil, nil))
	srv.SetSeerrWebhookSecret(cfg.Seerr.WebhookSecret)

	if _, err = database.InterruptRunningSweeps(ctx); err != nil {
		log.Error("interrupt running sweeps", "error", err)
		return err
	}
	handlers := map[string]card.Handler{"inc": svc.CardHandler()}
	renderers := map[string]card.Renderer{"inc": svc.CardRenderer()}
	defer startMaintenance(ctx, stop, cfg, bundle.clients, database, bot, srv, log, handlers, renderers)()
	defer startCards(ctx, bot, database, jrnl, handlers, renderers)()

	go svc.RecoverZombies(context.WithoutCancel(ctx))

	log.Info("media-fixer started")
	if err = srv.Start(ctx); err != nil {
		log.Error("server stopped", "error", err)
		return err
	}
	return nil
}
