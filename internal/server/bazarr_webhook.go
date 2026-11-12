package server

import (
	"crypto/subtle"
	"io"
	"net/http"
	"strconv"

	"github.com/minz1/mediafixer/internal/agent"
)

const maxBazarrBodyBytes = 64 << 10

func (s *Server) SetBazarrWebhook(secret string, waits *agent.BazarrWaits) {
	s.bazarrWebhookSecret = secret
	s.bazarrWaits = waits
}

func bazarrKey(r *http.Request) string {
	q := r.URL.Query()
	if n, err := strconv.Atoi(q.Get("ep")); err == nil {
		return "ep:" + strconv.Itoa(n)
	}
	if n, err := strconv.Atoi(q.Get("movie")); err == nil {
		return "movie:" + strconv.Itoa(n)
	}
	return ""
}

func (s *Server) handleBazarrWebhook(w http.ResponseWriter, r *http.Request) {
	if s.bazarrWebhookSecret == "" {
		http.Error(w, "bazarr webhook not configured", http.StatusServiceUnavailable)
		return
	}
	_, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.bazarrWebhookSecret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxBazarrBodyBytes))
	if key := bazarrKey(r); key != "" {
		s.bazarrWaits.Notify(key)
	}
	w.WriteHeader(http.StatusNoContent)
}
