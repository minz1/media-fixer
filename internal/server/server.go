package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

const readHeaderTimeout = 10 * time.Second

type Server struct {
	db      *db.DB
	svc     *incident.Service
	baseURL string
	log     *slog.Logger
	http    *http.Server
	tmpl    *dashboardTemplates
}

func New(addr, baseURL string, database *db.DB, svc *incident.Service, log *slog.Logger) (*Server, error) {
	tmpl, err := buildDashboardTemplate()
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}

	s := &Server{
		db:      database,
		svc:     svc,
		baseURL: baseURL,
		log:     log,
		tmpl:    tmpl,
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	r.Post("/ingest/seerr", s.handleSeerrWebhook)

	r.Route(baseURL, func(r chi.Router) {
		r.Get("/", s.dashboardIndex)
		r.Get("/incidents/{id}", s.dashboardIncident)

		r.Post("/incidents/{id}/resolve", s.actionResolve)
		r.Post("/incidents/{id}/reopen", s.actionReopen)
		r.Post("/incidents/{id}/reinvestigate", s.actionReinvestigate)
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
		return s.http.Shutdown(context.Background())
	}
}
