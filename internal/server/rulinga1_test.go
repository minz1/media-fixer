package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func TestRulingA1_CappedFindingOffersNoApprove(t *testing.T) {
	t.Parallel()
	srv, database := newEscalationTestServer(t, &stubEscalationAgent{})
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "The Boys"}
	lifecycletest.Incident(t, database, inc)
	capped := agent.DiagnosticResult{
		RootCause:        "bad release",
		Confidence:       "medium",
		PrimaryAction:    agent.EscalateManualInvestigation,
		PrimaryReason:    "the diagnosis contradicted itself",
		EscalateAction:   agent.EscalateManualInvestigation,
		RequiresApproval: true,
		Outcome: agent.DiagnosisOutcome{
			Kind:   agent.DiagnosisManual,
			Reason: "the diagnosis contradicted itself",
		},
	}
	if err := agent.CheckDiagnosis(&capped); err != nil {
		t.Fatal(err)
	}
	lifecycletest.Finding(t, database, inc.ID, capped)
	lifecycletest.MoveTo(t, database, inc.ID, db.StatusManualTestNeeded)

	snap, err := database.GetIncidentSnapshot(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []lifecycle.ActionKind{lifecycle.ActPreview, lifecycle.ActApprove, lifecycle.ActRefresh} {
		if lifecycle.Offers(snap, time.Now(), k) {
			t.Errorf("the card offers %s on a capped finding", k)
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/incidents/"+inc.ID, nil))
	body := rec.Body.String()
	for _, banned := range []string{"escalation-preview", "approve-escalation"} {
		if strings.Contains(body, banned) {
			t.Errorf("incident page offers %q on a capped finding", banned)
		}
	}
}
