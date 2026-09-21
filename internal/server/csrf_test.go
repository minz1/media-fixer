package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
)

func seedCSRFIncident(t *testing.T, database *db.DB) string {
	t.Helper()
	inc := &db.Incident{
		Status: db.StatusManualTestNeeded, Source: "discord", ReportedBy: "t",
		What: "cant_play", Title: "CSRF Fixture",
	}
	if err := database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	return inc.ID
}

// The dashboard's mutating routes are plain form POSTs behind Caddy's
// Authentik forward-auth, which is a session cookie. Whether a cross-site POST
// carries that cookie depends on a SameSite default this service neither sees
// nor controls — so the check has to be local. approve-escalation deletes
// media files, which is what makes it worth pinning.
func TestMutatingRoutes_RefuseCrossSiteRequests(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := seedCSRFIncident(t, database)

	paths := []string{
		"/media/pause",
		"/media/resume",
		"/media/incidents/" + inc + "/resolve",
		"/media/incidents/" + inc + "/rerun",
		"/media/incidents/" + inc + "/approve-escalation",
	}

	for _, path := range paths {
		for _, site := range []string{"cross-site", "same-site"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Sec-Fetch-Site", site)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s with Sec-Fetch-Site: %s = %d, want 403",
					path, site, rec.Code)
			}
		}
	}
}

// TestMutatingRoutes_AllowSameOriginAndDirectRequests is the other half: the
// guard must not break the dashboard's own hx-post requests, a direct form
// submission, or a command-line caller that sends neither header.
func TestMutatingRoutes_AllowSameOriginAndDirectRequests(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"direct navigation", map[string]string{"Sec-Fetch-Site": "none"}},
		{"no fetch metadata at all", nil},
		{"matching Origin", map[string]string{"Origin": "http://example.com"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/media/pause", nil)
			req.Host = "example.com"
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code == http.StatusForbidden {
				t.Errorf("POST /media/pause (%s) was refused; the dashboard's own "+
					"requests must still work", tc.name)
			}
		})
	}
}

// TestGETRoutesAreUnaffected keeps the guard off read-only routes, which
// change nothing and are reached by ordinary navigation.
func TestGETRoutesAreUnaffected(t *testing.T) {
	t.Parallel()
	srv, _ := newDashboardTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/media/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /media/ = %d, want 200: read-only routes are not a CSRF concern", rec.Code)
	}
}
