package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type sonarrKind string

const (
	sonarrGrab   sonarrKind = "grab"
	sonarrImport sonarrKind = "import"
	sonarrManual sonarrKind = "manual-interaction"
	sonarrTest   sonarrKind = "test"
	sonarrOther  sonarrKind = "other"
)

const (
	wakeDownload = "download"
	wakeGrabWait = "grab-wait"
	wakeStuck    = "stuck-queue"
)

type sonarrEpisode struct{ season, episode int }

type sonarrEvent struct {
	kind       sonarrKind
	title      string
	downloadID string
	episodes   []sonarrEpisode
}

type sonarrPayload struct {
	EventType string `json:"eventType"`
	Series    struct {
		Title string `json:"title"`
	} `json:"series"`
	Episodes []struct {
		SeasonNumber  int `json:"seasonNumber"`
		EpisodeNumber int `json:"episodeNumber"`
	} `json:"episodes"`
	DownloadID string `json:"downloadId"`
}

func parseSonarr(body []byte) (sonarrEvent, error) {
	var p sonarrPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return sonarrEvent{}, err
	}
	ev := sonarrEvent{kind: sonarrOther, title: p.Series.Title, downloadID: p.DownloadID}
	switch p.EventType {
	case "Grab":
		ev.kind = sonarrGrab
	case "Download":
		ev.kind = sonarrImport
	case "ManualInteractionRequired":
		ev.kind = sonarrManual
	case "Test":
		ev.kind = sonarrTest
	}
	for _, e := range p.Episodes {
		ev.episodes = append(ev.episodes, sonarrEpisode{season: e.SeasonNumber, episode: e.EpisodeNumber})
	}
	return ev, nil
}

func sonarrConsumers(k sonarrKind) []string {
	switch k {
	case sonarrGrab, sonarrImport:
		return []string{wakeDownload, wakeGrabWait}
	case sonarrManual:
		return []string{wakeDownload, wakeStuck}
	case sonarrTest, sonarrOther:
	}
	return nil
}

func (s *Server) SetSonarrWebhookSecret(secret string) {
	s.sonarrWebhookSecret = secret
}

func (s *Server) sonarrAuthorized(r *http.Request) bool {
	secret := []byte(s.sonarrWebhookSecret)
	if _, pass, ok := r.BasicAuth(); ok {
		return subtle.ConstantTimeCompare([]byte(pass), secret) == 1
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), secret) == 1
}

func (s *Server) handleSonarrWebhook(w http.ResponseWriter, r *http.Request) {
	if s.sonarrWebhookSecret == "" {
		http.Error(w, "sonarr webhook not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.sonarrAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSeerrBodyBytes))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ev, err := parseSonarr(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	for _, c := range sonarrConsumers(ev.kind) {
		s.wakeSonarrConsumer(ctx, c, ev)
	}
	s.log.InfoContext(ctx, "sonarr event", "kind", string(ev.kind), "title", ev.title, "download", ev.downloadID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) wakeSonarrConsumer(ctx context.Context, c string, ev sonarrEvent) {
	switch c {
	case wakeDownload:
		for _, e := range ev.episodes {
			if err := s.svc.ArmSonarrDownloadWake(ctx, "tv", ev.title, e.season, e.episode); err != nil {
				s.log.WarnContext(ctx, "sonarr webhook: download wake", "title", ev.title, "error", err)
			}
		}
	case wakeGrabWait:
		if s.sweep != nil {
			s.sweep.WakeDownload()
		}
	case wakeStuck:
		if s.stuck == nil {
			return
		}
		if err := s.stuck.RunNowIfDue(); err != nil {
			s.log.InfoContext(ctx, "sonarr webhook: stuck-queue wake refused", "error", err)
		}
	}
}
