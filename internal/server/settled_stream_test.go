package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/server"
)

func settledStreamFollowsRerun(t *testing.T, sp lifecycletest.Spec) {
	t.Helper()
	srv, database := newDashboardTestServer(t)
	s := lifecycletest.Seed(t, database, sp)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	events := openSSE(t, ts, "/media/incidents/"+s.ID+"/events")
	first, ok := nextSSE(t, events, sseEventDeadline)
	if !ok || strings.Contains(first, "event: settled") {
		t.Fatalf("first render of a %s incident: %q", sp.State, first)
	}
	if rec := do(t, srv, http.MethodPost, "/media/incidents/"+s.ID+"/rerun", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("rerun: %d %s", rec.Code, rec.Body.String())
	}
	next, ok := nextSSE(t, events, sseEventDeadline)
	if !ok || !strings.Contains(next, "re-opened") && !strings.Contains(next, "reopened") {
		t.Fatalf("the %s page did not follow the rerun: %q", sp.State, next)
	}
}

func TestD3_Blocked_StreamFollowsRerun(t *testing.T) {
	t.Parallel()
	settledStreamFollowsRerun(t, lifecycletest.Spec{Title: "Blocked", State: lifecycle.Blocked})
}

func TestD3_AgentFixed_StreamFollowsRerun(t *testing.T) {
	t.Parallel()
	settledStreamFollowsRerun(t, lifecycletest.Spec{Title: "Fixed", State: lifecycle.AgentFixed})
}

func TestD3_ManualTestNeeded_StreamFollowsRerun(t *testing.T) {
	t.Parallel()
	settledStreamFollowsRerun(t, lifecycletest.Spec{Title: "Needs you", State: lifecycle.ManualTestNeeded})
}

func TestD3_Resolved_StreamFollowsRerun(t *testing.T) {
	t.Parallel()
	settledStreamFollowsRerun(t, lifecycletest.Spec{Title: "Resolved", State: lifecycle.Resolved})
}

func TestD4_11_SettledPageKeepsStream(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	s := lifecycletest.Seed(t, database, lifecycletest.Spec{Title: "Done", State: lifecycle.Resolved})
	for _, path := range []string{"/media/incidents/" + s.ID, "/media/incidents/" + s.ID + "/transcript"} {
		body := do(t, srv, http.MethodGet, path, nil).Body.String()
		if !strings.Contains(body, "hx-sse:connect") || strings.Contains(body, "hx-sse:close") {
			t.Errorf("%s: the page closes its stream or has none", path)
		}
	}
}

func TestStatusReaders_UseMeta(t *testing.T) {
	t.Parallel()
	for _, st := range lifecycle.States() {
		m := lifecycle.Meta(st)
		if got := server.StatusTone(db.IncidentStatus(st)); got != string(m.Tone) {
			t.Errorf("%s tone %q, want %q", st, got, m.Tone)
		}
		if got := server.StatusWord(db.IncidentStatus(st)); got != m.Label {
			t.Errorf("%s word %q, want %q", st, got, m.Label)
		}
		if got := server.IncidentLive(db.IncidentStatus(st)); got != m.Live {
			t.Errorf("%s live %v, want %v", st, got, m.Live)
		}
	}
}
