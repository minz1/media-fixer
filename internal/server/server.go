package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

type Server struct {
	db      *db.DB
	svc     *incident.Service
	baseURL string
	log     *slog.Logger
	http    *http.Server
}

func New(addr, baseURL string, database *db.DB, svc *incident.Service, log *slog.Logger) *Server {
	s := &Server{
		db:      database,
		svc:     svc,
		baseURL: baseURL,
		log:     log,
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
		Addr:    addr,
		Handler: r,
	}
	return s
}

func (s *Server) Start(ctx context.Context) error {
	s.log.Info("http server starting", "addr", s.http.Addr, "base", s.baseURL)
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
