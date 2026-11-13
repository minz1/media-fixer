package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestD7_DashboardSwapPreview(t *testing.T) {
	t.Parallel()
	plan := &client.SwapPlan{
		What:     agent.WhatSubtitles,
		Language: "eng",
		Title:    "Show",
		Season:   1,
		Episode:  6,
		Current:  client.SwapCurrent{SwapFile: client.SwapFile{Path: "/data/tv/Show/e6.mkv"}},
		Candidate: client.SwapCandidate{
			SwapFile:   client.SwapFile{Path: "/data/downloads/sonarr/New/e6.mkv"},
			GraceHours: 168,
		},
		Blocklist:  client.SwapBlocklist{SourceTitle: "Show.S01E06.OLD"},
		RecycleBin: "/recycle",
	}
	srv, database := newEscalationTestServer(t, &stubEscalationAgent{planResult: plan})
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: agent.WhatSubtitles, Title: "Show"}
	lifecycletest.Incident(t, database, inc)
	lifecycletest.Finding(t, database, inc.ID, agent.DiagnosticResult{
		RootCause: "bad subs", EscalateAction: agent.EscalateSwapRelease, RequiresApproval: true,
	})
	lifecycletest.MoveTo(t, database, inc.ID, db.StatusManualTestNeeded)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/media/incidents/"+inc.ID+"/escalation-preview?v="+curV(t, database, inc.ID), nil,
	))
	body := rec.Body.String()
	for _, want := range []string{
		"Sonarr deletes this file as part of the import", "/recycle", "/data/downloads/sonarr/New/e6.mkv",
		"new Jellyfin item", "Show.S01E06.OLD", "Not automatic", "approve-escalation", `name="plan_fp"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q:\n%s", want, body)
		}
	}
}
