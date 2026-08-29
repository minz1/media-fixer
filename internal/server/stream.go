package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

// sseKeepAliveInterval bounds how long an idle SSE connection goes without a
// byte crossing the wire. Comfortably under any reverse proxy's own idle
// timeout (nginx defaults to 60s) so admin.minz1.com/media's connection
// isn't silently dropped mid-incident.
const sseKeepAliveInterval = 20 * time.Second

// startSSE writes the response headers an event stream needs and performs
// the first flush, confirming the connection is actually streamable before
// the caller starts writing events. X-Accel-Buffering: no matters
// specifically because this dashboard is proxied (admin.minz1.com/media) —
// without it nginx buffers the whole response and nothing arrives until the
// connection closes, silently turning "live" into "eventually." Returns
// ok=false if the underlying ResponseWriter can't be flushed at all (should
// not happen with net/http's own server, but a defensive check costs
// nothing and avoids a confusing partial stream).
func startSSE(w http.ResponseWriter) (func(), bool) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return nil, false
	}
	return func() { _ = rc.Flush() }, true
}

// writeSSEEvent writes one SSE message. seq, when > 0, becomes the event's
// id: field, so a reconnecting client's Last-Event-ID (sent automatically by
// the browser's EventSource, and by htmx 4's hx-sse) reflects how far it got.
// data is split on newlines and each line prefixed with "data: " per the SSE
// wire format — this is also how multiple <hx-partial> elements are packed
// into one message: the caller joins several partials with "\n" and each
// becomes its own data: line, which htmx concatenates back with newlines and
// scans as one HTML fragment before swapping.
func writeSSEEvent(w http.ResponseWriter, flush func(), seq int64, data string) {
	if seq > 0 {
		fmt.Fprintf(w, "id: %d\n", seq)
	}
	for line := range strings.SplitSeq(data, "\n") {
		// data is our own server-rendered HTML, already escaped by
		// html/template when the fragment was executed — not raw user
		// input reaching the wire unescaped.
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
	flush()
}

// writeSSENamedEvent writes an event with an explicit "event:" field, for
// hx-sse:close="<name>" on the client to match against.
func writeSSENamedEvent(w http.ResponseWriter, flush func(), name, data string) {
	fmt.Fprintf(w, "event: %s\n", name)
	for line := range strings.SplitSeq(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
	flush()
}

// writeSSEComment writes a ":"-prefixed comment line, invisible to the
// client's event handlers, purely to keep the connection active across
// sseKeepAliveInterval-long idle gaps.
func writeSSEComment(w http.ResponseWriter, flush func(), comment string) {
	fmt.Fprintf(w, ": %s\n\n", comment)
	flush()
}

// hxPartial wraps html in an <hx-partial> element targeting id, for
// packing into a multi-region SSE update (see writeSSEEvent).
func hxPartial(id, html string) string {
	return fmt.Sprintf(`<hx-partial hx-target="#%s" hx-swap="outerHTML">%s</hx-partial>`, id, html)
}

// dashboardEvents streams live-update signals for the incident index page:
// any event from any incident (see journal.SubscribeAll) re-renders the
// current incident list. There is no per-incident history to replay here —
// unlike the incident page, this view only ever shows current state, so a
// fresh reconnect just re-renders once immediately rather than needing
// Last-Event-ID replay.
func (s *Server) dashboardEvents(w http.ResponseWriter, r *http.Request) {
	if s.journal == nil {
		http.Error(w, "event stream not configured", http.StatusServiceUnavailable)
		return
	}
	flush, ok := startSSE(w)
	if !ok {
		return
	}
	ctx := r.Context()

	render := func() {
		paused, _ := s.db.IsAutonomousPaused(ctx)
		incidents, err := s.db.ListIncidents(ctx, "", dashboardPageSize, 0)
		if err != nil {
			return
		}
		var sb strings.Builder
		_ = s.tmpl.t.ExecuteTemplate(&sb, "incidents_list", map[string]any{
			"Incidents":   incidents,
			"Paused":      paused,
			tplKeyBaseURL: s.baseURL,
		})
		writeSSEEvent(w, flush, 0, hxPartial("incidents-list", sb.String()))
	}
	render()

	ch, unsubscribe := s.journal.SubscribeAll()
	defer unsubscribe()

	ticker := time.NewTicker(sseKeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, chOK := <-ch:
			if !chOK {
				return
			}
			render()
		case <-ticker.C:
			writeSSEComment(w, flush, "keep-alive")
		}
	}
}

// incidentEvents streams live updates for one incident: on connect and on
// every subsequent event (any kind — see the doc comment on
// buildIncidentPageData), it re-renders the incident-card, incident-actions,
// and incident-transcript regions from a fresh read of current state, sent
// together as one SSE message via hxPartial. A full re-render rather than an
// incremental diff means a client that reconnects mid-run (or one that just
// missed a coalesced/dropped notification — see journal.Journal.Subscribe's
// non-blocking-send contract) is trivially always consistent: there is no
// delta to have missed. Closes the stream with a named "settled" event once
// the incident reaches a terminal status with nothing left pending, which the
// incident page's hx-sse:close="settled" matches to stop reconnecting.
func (s *Server) incidentEvents(w http.ResponseWriter, r *http.Request) {
	id := incidentIDParam(r)
	if s.journal == nil {
		http.Error(w, "event stream not configured", http.StatusServiceUnavailable)
		return
	}
	// ?full=1 (set by the transcript_page template) means this connection
	// belongs to the full thought-process page, which has no
	// incident-card/incident-actions elements to target — send only the
	// incident-transcript partial, rendered with Full=true.
	full := r.URL.Query().Get("full") == "1"
	flush, ok := startSSE(w)
	if !ok {
		return
	}
	ctx := r.Context()

	render := func() bool {
		data, err := s.buildIncidentPageData(ctx, id, full)
		if err != nil {
			return false
		}
		combined, renderErr := s.renderIncidentFragments(data, full)
		if renderErr != nil {
			return false
		}
		writeSSEEvent(w, flush, 0, combined)
		inc, _ := data["Incident"].(*db.Incident)
		return inc != nil && incidentSettled(inc)
	}
	if render() {
		writeSSENamedEvent(w, flush, "settled", "")
		return
	}

	ch, unsubscribe := s.journal.Subscribe(id)
	defer unsubscribe()

	ticker := time.NewTicker(sseKeepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, chOK := <-ch:
			if !chOK {
				return
			}
			if render() {
				writeSSENamedEvent(w, flush, "settled", "")
				return
			}
		case <-ticker.C:
			writeSSEComment(w, flush, "keep-alive")
		}
	}
}

// renderIncidentFragments executes the live regions of the incident page (or,
// for the full thought-process page, just the transcript) against data and
// packs them into one combined SSE payload.
func (s *Server) renderIncidentFragments(data map[string]any, transcriptOnly bool) (string, error) {
	targets := map[string]string{"incident-transcript": "incident_transcript"}
	if !transcriptOnly {
		targets["incident-card"] = "incident_card"
		targets["incident-actions"] = "incident_actions"
	}
	var parts []string
	for id, name := range targets {
		var sb strings.Builder
		if err := s.tmpl.t.ExecuteTemplate(&sb, name, data); err != nil {
			return "", err
		}
		parts = append(parts, hxPartial(id, sb.String()))
	}
	return strings.Join(parts, "\n"), nil
}

// incidentSettled reports whether an incident has reached a terminal state
// with nothing left for the sweep loops to advance — the point at which the
// live stream has nothing further to say and can close instead of holding
// the connection open indefinitely. Deliberately does not consult
// PendingOutcome: FindDuePendingOutcomes only sweeps StatusVerifying
// incidents (see its doc comment), so a pending outcome can only exist
// alongside a non-terminal status in the first place — checking status alone
// is already correct.
func incidentSettled(inc *db.Incident) bool {
	switch inc.Status {
	case db.StatusResolved, db.StatusAgentFixed, db.StatusManualTestNeeded, db.StatusBlocked:
		return true
	case db.StatusOpen, db.StatusInvestigating, db.StatusVerifying, db.StatusReopened:
		return false
	default:
		return false
	}
}
