package server

import (
	"net/http"

	"github.com/minz1/mediafixer/internal/livecheck"
)

// selftestIndex renders the /selftest page: the last cached report (if any)
// plus the run form. Nothing is executed on a plain page load, so opening
// the page never itself hits the live services.
func (s *Server) selftestIndex(w http.ResponseWriter, _ *http.Request) {
	s.reportMu.Lock()
	report := s.lastReport
	s.reportMu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "selftest", map[string]any{
		tplKeyBaseURL: s.baseURL,
		"Configured":  s.checker != nil,
		"Report":      report,
	})
}

// selftestRun executes the live-check suite and returns the results table
// fragment for htmx to swap in. Disruptive checks are never reachable from
// the web — only the CLI can pass -disruptive. "Include safe writes" maps to
// AllowWrite.
//
// Single-flighted via s.checkRunning: two overlapping runs would each call
// write-tier decypharr actions independently and race into decypharr's own
// single-flight repair lock, producing a 409 there that looks like a
// livecheck bug rather than the self-inflicted double-fire it actually is.
func (s *Server) selftestRun(w http.ResponseWriter, r *http.Request) {
	if s.checker == nil {
		http.Error(w, "live checks not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.checkRunning.CompareAndSwap(false, true) {
		http.Error(w, "a check run is already in progress", http.StatusConflict)
		return
	}
	defer s.checkRunning.Store(false)

	opts := livecheck.Options{AllowWrite: r.FormValue("write") == "on"}
	report := livecheck.New(s.checker, opts).Run(r.Context())

	s.reportMu.Lock()
	s.lastReport = report
	s.reportMu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.t.ExecuteTemplate(w, "selftest_table", report)
}
