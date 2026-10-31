package server

import (
	"context"
	"io"
	"net"

	"github.com/minz1/mediafixer/internal/stuckqueue"
)

// SeerrIssueTypeToWhat exports the internal mapping for use in package server_test.
func SeerrIssueTypeToWhat(s string) string { return seerrIssueTypeToWhat(s) }

// BuildIncidentPageDataForTest exposes buildIncidentPageData for direct
// template-execution-error tests (see dashboard_test.go) — production
// handlers discard ExecuteTemplate's error, so an HTTP-level test alone could
// miss a template bug that only manifests partway through rendering.
func (s *Server) BuildIncidentPageDataForTest(ctx context.Context, id string, full bool) (map[string]any, error) {
	return s.buildIncidentPageData(ctx, id, full)
}

// ExecuteTemplateForTest exposes the parsed template set's ExecuteTemplate
// with its error propagated, instead of the production handlers' `_ =`.
func (s *Server) ExecuteTemplateForTest(w io.Writer, name string, data any) error {
	return s.tmpl.t.ExecuteTemplate(w, name, data)
}

func (s *Server) UseListenerForTest(l net.Listener) { s.listener = l }

func (s *Server) StuckCardDataForTest(
	res stuckqueue.Result,
	set stuckqueue.Settings,
	unreadable string,
) map[string]any {
	return s.stuckCardData(&res, false, set, nil, "", "", unreadable)
}

func (s *Server) TimersCardForTest(ctx context.Context) map[string]any { return s.timersCardFor(ctx) }
