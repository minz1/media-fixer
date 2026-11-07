package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/babysit"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/clientset"
	"github.com/minz1/mediafixer/internal/config"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/opexec"
	"github.com/minz1/mediafixer/internal/server"
	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/stuckread"
	"github.com/minz1/mediafixer/internal/sweep"
)

func main() {
	if code := exitCode(run()); code != 0 {
		os.Exit(code)
	}
}

const exitDownRefused = 2

func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, db.ErrDownRefused):
		return exitDownRefused
	}
	return 1
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
	disp := clients.Dispatcher()

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
	srv *server.Server, log *slog.Logger, renderers map[string]card.Renderer,
) *sweep.Scheduler {
	if sonarrURL == "" {
		log.WarnContext(ctx, "sonarr not configured — pack sweep unavailable")
		return nil
	}
	r := &sweep.Runner{
		Sonarr: sonarr, DB: database, Notify: bot.NotifyOwner, Refresh: bot.Refresh,
		Now: time.Now, Sleep: sweep.Sleep, Log: log,
	}
	renderers["sweep"] = r.CardRenderer()
	sch := sweep.NewScheduler(ctx, r, database)
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
	if err := sch.LoadOperations(ctx); err != nil {
		log.ErrorContext(ctx, "load stuck-queue operations", "error", err)
	}
	sch.Refresh = bot.Refresh
	handlers["sq"], renderers["sq"] = sch.CardHandler(), sch.CardRenderer()
	srv.SetStuckQueue(sch)
	sch.Start()
	return sch
}

func newStuckRead(clients *clientset.Set, svc *incident.Service, database *db.DB, log *slog.Logger) *stuckread.Watcher {
	if clients.MediaAgent == nil {
		return nil
	}
	sr := stuckread.New(clients.MediaAgent, svc, database, log)
	svc.SetStuckRead(sr)
	return sr
}

func startMaintenance(
	ctx context.Context, cfg *config.Config, bundle *agentBundle, database *db.DB,
	bot *discord.Bot, srv *server.Server, log *slog.Logger,
	handlers map[string]card.Handler, renderers map[string]card.Renderer, sr *stuckread.Watcher,
) []step {
	clients := bundle.clients
	sweepSch := startSweep(ctx, cfg.Sonarr.URL, clients.Sonarr, database, bot, srv, log, renderers)
	stuckSch := startStuckQueue(ctx, cfg, clients, database, bot, srv, log, handlers, renderers)
	if sweepSch != nil && stuckSch != nil {
		srv.SetBabysit(&babysit.Service{
			DB: database, Sonarr: clients.Sonarr, State: stuckSch.State, LLM: bundle.summary,
			Model: cfg.LLM.Model, Now: time.Now, Log: log,
		})
	}
	var waits []step
	if sr != nil {
		sr.Start(ctx)
		waits = append(waits, step{name: "stuckread", run: within(sr.Wait)})
	}
	if sweepSch != nil {
		waits = append(waits, step{name: "sweep", run: within(sweepSch.Wait)})
	}
	if stuckSch != nil {
		waits = append(waits, step{name: "stuckqueue", run: within(stuckSch.Wait)})
	}
	return waits
}

func startCards(
	ctx context.Context, bot *discord.Bot, database *db.DB, jrnl *journal.Journal,
	handlers map[string]card.Handler, renderers map[string]card.Renderer,
) func() {
	ch, unsub := jrnl.SubscribeAll()
	bot.StartCards(ctx, database, ch, handlers, renderers)
	return unsub
}

type app struct {
	cfg      *config.Config
	log      *slog.Logger
	database *db.DB
	jrnl     *journal.Journal
	bundle   *agentBundle
	bot      *discord.Bot
	svc      *incident.Service
	srv      *server.Server
	sr       *stuckread.Watcher
}

func run() error {
	cfgPath := flag.String("config", "/etc/media-fixer/config.toml", "path to TOML config file")
	migrateDown := flag.Int("migrate-down", 0, "roll back this schema migration, print the counts and exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "error", err)
		return err
	}

	if *migrateDown != 0 {
		if err = db.MigrateDown(context.Background(), cfg.DB.Path, *migrateDown, os.Stdout); err != nil {
			log.Error("migrate down", "error", err)
		}
		return err
	}

	database, err := db.Open(cfg.DB.Path)
	if errors.Is(err, db.ErrLocked) {
		log.Error(err.Error())
		return err
	}
	if err != nil {
		log.Error("open database", "error", err)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := build(ctx, cfg, database, log)
	if err != nil {
		_ = database.Close()
		return err
	}
	return a.serve(ctx, stop)
}

func build(ctx context.Context, cfg *config.Config, database *db.DB, log *slog.Logger) (*app, error) {
	a := &app{cfg: cfg, log: log, database: database, jrnl: journal.New(database)}
	bundle, err := buildAgentComponents(cfg, database, a.jrnl, log)
	if err != nil {
		log.ErrorContext(ctx, "build agent components", "error", err)
		return nil, err
	}
	a.bundle = bundle

	a.bot, err = discord.New(cfg.Discord.Token, cfg.Discord.GuildID, cfg.Discord.OwnerID, cfg.Server.PublicURL, log)
	if err != nil {
		log.ErrorContext(ctx, "init discord bot", "error", err)
		return nil, err
	}

	a.svc = incident.NewService(ctx, database, a.jrnl, bundle.ag, bundle.ctrl, bundle.summary, a.bot, log)
	if cfg.Seerr.URL != "" {
		a.svc.SetSeerr(client.NewSeerr(cfg.Seerr.URL, cfg.Seerr.APIKey))
	} else {
		log.WarnContext(ctx, "seerr not configured — seerr reporters get no issue updates")
	}
	a.svc.SetOpPorts(opPorts(bundle.clients))
	a.sr = newStuckRead(bundle.clients, a.svc, database, log)
	a.bot.SetService(a.svc)

	a.srv, err = server.New(cfg.Server.Addr, cfg.Server.BaseURL, database, a.jrnl, a.svc, log)
	if err != nil {
		log.ErrorContext(ctx, "init server", "error", err)
		return nil, err
	}
	a.srv.SetChecker(bundle.clients.Dispatcher())
	a.srv.SetSeerrWebhookSecret(cfg.Seerr.WebhookSecret)

	if _, err = database.InterruptRunningSweeps(ctx); err != nil {
		log.ErrorContext(ctx, "interrupt running sweeps", "error", err)
		return nil, err
	}
	return a, nil
}

func (a *app) serve(ctx context.Context, stop context.CancelFunc) error {
	handlers := map[string]card.Handler{"inc": a.svc.CardHandler()}
	renderers := map[string]card.Renderer{"inc": a.svc.CardRenderer()}
	var maintenance []step
	var unsub func()
	signalled := make(chan time.Time, 1)
	context.AfterFunc(ctx, func() { signalled <- time.Now() })
	startErr := startup(startSteps{
		interrupt: func() error { return interruptOperations(ctx, a.database, a.log) },
		maintenance: func() {
			maintenance = startMaintenance(
				ctx, a.cfg, a.bundle, a.database, a.bot, a.srv, a.log, handlers, renderers, a.sr)
		},
		cards:    func() { unsub = startCards(ctx, a.bot, a.database, a.jrnl, handlers, renderers) },
		bot:      func() { a.bot.Start(ctx) },
		intake:   func() { context.AfterFunc(ctx, a.bot.StopIntake) },
		service:  a.svc.Start,
		recovery: a.svc.StartRecovery,
	})
	if startErr != nil {
		stop()
		_ = a.database.Close()
		return startErr
	}

	a.log.InfoContext(ctx, "media-fixer started")
	httpErr := a.srv.Start(ctx)
	if httpErr != nil && !errors.Is(httpErr, server.ErrDrainTimeout) {
		a.log.ErrorContext(ctx, "server stopped", "error", httpErr)
	}
	stop()
	sctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), (<-signalled).Add(shutdownBudget))
	defer cancel()
	shutdown(sctx, a.log, shutdownPlan{
		intake: a.bot.StopIntake,
		cancel: func() {
			stop()
			a.svc.Stop()
		},
		http: httpErr,
		waits: append([]step{
			{name: "discord", run: a.bot.WaitHandlers},
			{name: "incident", run: a.svc.Shutdown},
		}, maintenance...),
		subs: []step{
			{name: "discord session", run: a.bot.Disconnect},
			{name: "journal", run: plain(unsub)},
		},
		closeDB: a.database.Close,
	})
	if httpErr != nil && !errors.Is(httpErr, server.ErrDrainTimeout) {
		return httpErr
	}
	return nil
}

func opPorts(clients *clientset.Set) opexec.Ports {
	return opexec.Ports{
		Arr: func(app string) opexec.Arr {
			switch {
			case app == "sonarr" && clients.Sonarr != nil:
				return clients.Sonarr
			case app == "radarr" && clients.Radarr != nil:
				return clients.Radarr
			}
			return nil
		},
		Decypharr: clients.Decypharr,
	}
}

func interruptOperations(ctx context.Context, database *db.DB, log *slog.Logger) error {
	ops, err := database.InterruptRunningOperations(ctx, time.Now())
	if err != nil {
		log.ErrorContext(ctx, "interrupt running operations", "error", err)
		return err
	}
	msg := fmt.Sprintf("operations: interrupted %d at startup", len(ops))
	if len(ops) == 0 {
		log.InfoContext(ctx, msg)
		return nil
	}
	log.WarnContext(ctx, msg)
	for _, op := range ops {
		log.WarnContext(ctx, fmt.Sprintf("operations: interrupted op=%d subject=%s:%s step=%d",
			op.ID, op.Subject.Kind, op.Subject.ID, op.Index))
	}
	return nil
}
