package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/livecheck"
)

const readHeaderTimeout = 10 * time.Second

// shutdownGrace bounds how long Shutdown waits for connections to drain.
// http.Server.Shutdown does NOT cancel in-flight request contexts, and an SSE
// handler only returns when its request context is done — so without a bound
// here, a single open dashboard tab makes Shutdown block forever and systemd
// SIGKILLs the unit at TimeoutStopSec. The streaming handlers also select on
// Server.shutdown so they exit promptly rather than riding this timeout out.
const shutdownGrace = 5 * time.Second

type Server struct {
	db      *db.DB
	journal *journal.Journal
	svc     *incident.Service
	baseURL string
	log     *slog.Logger
	http    *http.Server
	tmpl    *dashboardTemplates

	// checker and lastReport back the /selftest dashboard page. checker is
	// nil until SetChecker is called (e.g. if the media-agent dependency
	// isn't configured at all), in which case the page reports live checks
	// as unavailable rather than 500ing.
	checker    *agent.Dispatcher
	reportMu   sync.Mutex
	lastReport *livecheck.Report
	// checkRunning single-flights selftestRun: two overlapping runs (a
	// double-click, a page retry) would each independently call
	// refresh_decypharr_links/decypharr_repair_sweep and race each other
	// into decypharr's own single-flight repair lock, producing a confusing
	// 409 that looks like a livecheck bug rather than a self-inflicted
	// double-fire. Confirmed live via decypharr's persisted run history
	// showing two runs 5s apart from the same source.
	checkRunning atomic.Bool

	// shutdown is closed once Start begins shutting down, telling the
	// long-lived SSE handlers to return so Shutdown can drain. Closed exactly
	// once, by Start.
	shutdown chan struct{}
}

// SetChecker wires the dispatcher the /selftest page runs checks against.
// Called once from main after the dispatcher is built; a nil checker (the
// zero value before this is called) is handled gracefully by the handlers.
func (s *Server) SetChecker(disp *agent.Dispatcher) {
	s.checker = disp
}

func New(
	addr, baseURL string, database *db.DB, jrnl *journal.Journal, svc *incident.Service, log *slog.Logger,
) (*Server, error) {
	tmpl, err := buildDashboardTemplate()
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}

	s := &Server{
		db:       database,
		journal:  jrnl,
		svc:      svc,
		baseURL:  baseURL,
		log:      log,
		tmpl:     tmpl,
		shutdown: make(chan struct{}),
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Post("/ingest/seerr", s.handleSeerrWebhook)

	r.Route(baseURL, func(r chi.Router) {
		r.Get("/", s.dashboardIndex)
		r.Get("/events", s.dashboardEvents)
		r.Get("/incidents/{id}", s.dashboardIncident)
		r.Get("/incidents/{id}/events", s.incidentEvents)
		r.Get("/incidents/{id}/transcript", s.incidentTranscript)
		r.Get("/incidents/{id}/export", s.incidentExport)

		r.Post("/incidents/{id}/resolve", s.actionResolve)
		r.Post("/incidents/{id}/rerun", s.actionRerun)
		r.Post("/incidents/{id}/keep-searching", s.actionKeepSearching)
		r.Post("/incidents/{id}/unlock", s.actionUnlock)
		r.Get("/incidents/{id}/escalation-preview", s.escalationPreview)
		r.Post("/incidents/{id}/approve-escalation", s.approveEscalation)
		r.Get("/selftest", s.selftestIndex)
		r.Post("/selftest/run", s.selftestRun)
		r.Post("/pause", s.actionPause)
		r.Post("/resume", s.actionResume)
	})

	s.http = &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return s, nil
}

// Handler returns the underlying HTTP handler, for use in tests.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

func (s *Server) Start(ctx context.Context) error {
	s.log.InfoContext(ctx, "http server starting", "addr", s.http.Addr, "base", s.baseURL)
	errCh := make(chan error, 1)
	go func() {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		close(s.shutdown)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	}
}
