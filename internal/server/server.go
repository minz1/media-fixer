package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/babysit"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/discord"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/livecheck"
	"github.com/minz1/mediafixer/internal/stuckqueue"
	"github.com/minz1/mediafixer/internal/sweep"
)

const readHeaderTimeout = 10 * time.Second

var ErrDrainTimeout = errors.New("http drain did not finish")

// shutdownGrace bounds how long [http.Server.Shutdown] waits for connections to drain.
// [http.Server.Shutdown] does NOT cancel in-flight request contexts, and an SSE
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
	checkRunning    atomic.Bool
	selftestTimeout time.Duration

	// shutdown is closed once Start begins shutting down, telling the
	// long-lived SSE handlers to return so Shutdown can drain. Closed exactly
	// once, by Start.
	shutdown chan struct{}

	seerrWebhookSecret  string
	sonarrWebhookSecret string

	bazarrWebhookSecret string
	bazarrWaits         *agent.BazarrWaits

	sweep   *sweep.Scheduler
	stuck   *stuckqueue.Scheduler
	babysit *babysit.Service

	listener net.Listener

	discord  atomic.Pointer[func() discord.Conn]
	jellyfin atomic.Pointer[func() client.JellyfinWSState]
}

func (s *Server) SetJellyfinEvents(state func() client.JellyfinWSState) {
	s.jellyfin.Store(&state)
}

func (s *Server) jellyfinView() connView {
	f := s.jellyfin.Load()
	if f == nil {
		return connView{}
	}
	st := (*f)()
	if st.Connected {
		return connView{Word: "Jellyfin events connected", Tone: toneGreen}
	}
	if st.Err == "" {
		return connView{Word: "Jellyfin events connecting", Tone: toneYellow}
	}
	info := st.Err
	if !st.RetryAt.IsZero() {
		info = "Retrying at " + st.RetryAt.UTC().Format("15:04:05 MST") + ": " + st.Err
	}
	return connView{Word: "Jellyfin events offline", Tone: toneRed, Info: info}
}

func (s *Server) SetDiscord(conn func() discord.Conn) {
	s.discord.Store(&conn)
}

func (s *Server) discordView() connView {
	f := s.discord.Load()
	if f == nil {
		return connView{}
	}
	return newConnView((*f)(), time.Now())
}

// SetChecker wires the dispatcher the /selftest page runs checks against.
// Called once from main after the dispatcher is built; a nil checker (the
// zero value before this is called) is handled gracefully by the handlers.
func (s *Server) SetChecker(disp *agent.Dispatcher) {
	s.checker = disp
}

func (s *Server) SetSeerrWebhookSecret(secret string) {
	s.seerrWebhookSecret = secret
}

func New(
	addr, baseURL string, database *db.DB, jrnl *journal.Journal, svc *incident.Service, log *slog.Logger,
) (*Server, error) {
	s := &Server{
		db:       database,
		journal:  jrnl,
		svc:      svc,
		baseURL:  baseURL,
		log:      log,
		shutdown: make(chan struct{}),
	}
	tmpl, err := buildDashboardTemplate(s.discordView, s.jellyfinView)
	if err != nil {
		return nil, fmt.Errorf("parse dashboard template: %w", err)
	}
	s.tmpl = tmpl

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	r.Post("/ingest/seerr", s.handleSeerrWebhook)
	r.Post("/ingest/sonarr", s.handleSonarrWebhook)
	r.Post("/ingest/bazarr", s.handleBazarrWebhook)

	r.Route(baseURL, func(r chi.Router) {
		// Every mutating route below is an unauthenticated-from-this-service's
		// point of view form POST; auth lives in Caddy's Authentik
		// forward_auth, which is cookie-based. See requireSameOrigin.
		r.Use(requireSameOrigin)
		r.Use(s.refuseWhileClosing)

		r.Get("/", s.dashboardIndex)
		r.Get("/events", s.dashboardEvents)
		r.Get("/incidents/{id}", s.dashboardIncident)
		r.Get("/incidents/{id}/events", s.incidentEvents)
		r.Get("/incidents/{id}/transcript", s.incidentTranscript)
		r.Get("/incidents/{id}/export", s.incidentExport)

		r.Post("/incidents/{id}/resolve", s.actionResolve)
		r.Post("/incidents/{id}/rerun", s.actionRerun)
		r.Post("/incidents/{id}/resume", s.actionResumeOperation)
		r.Post("/incidents/{id}/keep-searching", s.actionKeepSearching)
		r.Post("/incidents/{id}/unlock", s.actionUnlock)
		r.Get("/incidents/{id}/escalation-preview", s.escalationPreview)
		r.Post("/incidents/{id}/approve-escalation", s.approveEscalation)
		r.Get("/selftest", s.selftestIndex)
		r.Post("/selftest/run", s.selftestRun)
		r.Post("/pause", s.actionPause)
		r.Post("/resume", s.actionResume)
		r.Get("/maintenance", s.maintenanceIndex)
		r.Post("/maintenance/pack-sweep/settings", s.sweepSettingsSave)
		r.Post("/maintenance/pack-sweep/run", s.sweepRun)
		r.Get("/maintenance/pack-sweep/status", s.sweepStatusFragment)
		r.Post("/maintenance/pack-sweep/refused/{id}/forget", s.sweepForget)
		r.Get("/maintenance/stuck-queue/results", s.stuckResults)
		r.Post("/maintenance/stuck-queue/settings", s.stuckSettingsSave)
		r.Post("/maintenance/stuck-queue/run", s.stuckRun)
		r.Post("/maintenance/stuck-queue/resume", s.stuckResume)
		r.Post("/maintenance/stuck-queue/{action}", s.stuckAction)
		r.Post("/maintenance/incident-timers/settings", s.timersSettingsSave)
		r.Get("/monitoring", s.monitorIndex)
		r.Get("/monitoring/{seriesID}", s.monitorShow)
		r.Post("/monitoring/{seriesID}/search/{pass}", s.monitorSearch)
		r.Post("/monitoring/{seriesID}/explain", s.monitorExplain)
		r.Post("/monitoring/{seriesID}/import/propose", s.monitorPropose)
		r.Post("/monitoring/{seriesID}/import", s.monitorImport)
		r.Post("/monitoring/{seriesID}/releases", s.monitorReleases)
		r.Post("/monitoring/{seriesID}/grab", s.monitorGrab)
		r.Post("/monitoring/exclude", s.monitorSetExcluded(true))
		r.Post("/monitoring/include", s.monitorSetExcluded(false))
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
		var err error
		if s.listener != nil {
			err = s.http.Serve(s.listener)
		} else {
			err = s.http.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
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
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("%w: %w", ErrDrainTimeout, err)
		}
		return nil
	}
}

func (s *Server) closing() bool {
	select {
	case <-s.shutdown:
		return true
	default:
		return false
	}
}

func (s *Server) refuseWhileClosing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && s.closing() {
			http.Error(w, card.RestartingNotice, http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}
