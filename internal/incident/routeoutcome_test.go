package incident_test

import (
	"context"
	"errors"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func fixNowFinding() *agent.DiagnosticResult {
	return &agent.DiagnosticResult{
		RootCause: "x", Confidence: "high", PrimaryAction: "run-a", PrimaryReason: "x",
		Outcome: agent.DiagnosisOutcome{Kind: agent.DiagnosisFixNow},
	}
}

func routeSnap(t *testing.T, fx *escalationFixture, id string) lifecycle.Snapshot {
	t.Helper()
	s, err := fx.db.GetIncidentSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func routeFixtureWhat(t *testing.T, what string) (*escalationFixture, *captureNotifier, *db.Incident) {
	t.Helper()
	notif := &captureNotifier{}
	fx := newEscalationFixtureNotifier(t, newEscalationAgent(), notif)
	inc := &db.Incident{Status: db.StatusInvestigating, Source: "discord", What: what, Title: "The Boys"}
	lifecycletest.Incident(t, fx.db, inc)
	return fx, notif, inc
}

func TestRouteExit_StateFirstKeepsP2Reasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state  agent.RunState
		err    error
		status lifecycle.State
		reason lifecycle.Reason
		parked bool
	}{
		{agent.RunBudgetTripped, nil, lifecycle.ManualTestNeeded, lifecycle.ReasonBudgetSpent, false},
		{agent.RunLLMFailed, errors.New("boom"), lifecycle.ManualTestNeeded, lifecycle.ReasonAgentError, false},
		{agent.RunBatchFailed, errors.New("boom"), lifecycle.ManualTestNeeded, lifecycle.ReasonAgentError, false},
		{agent.RunSuperseded, context.Canceled, lifecycle.Investigating, lifecycle.ReasonNone, false},
		{agent.RunPaused, nil, lifecycle.Reopened, lifecycle.ReasonNone, true},
	}
	for _, c := range cases {
		t.Run(string(c.state), func(t *testing.T) {
			t.Parallel()
			fx, _, inc := routeFixture(t)
			fx.svc.RouteExitForTest(context.Background(), inc, agent.RunExit{
				State: c.state, Err: c.err, Finding: fixNowFinding(), Started: true,
			})
			s := routeSnap(t, fx, inc.ID)
			if s.Status != c.status || s.Reason != c.reason || s.Parked != c.parked {
				t.Errorf("%s → %s/%q parked=%v, want %s/%q parked=%v",
					c.state, s.Status, s.Reason, s.Parked, c.status, c.reason, c.parked)
			}
		})
	}
}

func TestRouteExit_OutcomeKindsMapToLifecycle(t *testing.T) {
	t.Parallel()
	target := agent.SearchTarget{MediaType: "tv", Title: "The Boys", Season: 1, Episode: 5}
	outcomes := map[agent.DiagnosisKind]func(r *agent.DiagnosticResult){
		agent.DiagnosisFixNow: func(*agent.DiagnosticResult) {},
		agent.DiagnosisDeferredVerify: func(r *agent.DiagnosticResult) {
			r.VerifyAfterSeconds = 60
			r.Outcome = agent.DiagnosisOutcome{Kind: agent.DiagnosisDeferredVerify, AfterSeconds: 60}
		},
		agent.DiagnosisAwaitDownload: func(r *agent.DiagnosticResult) {
			r.PrimaryAction = agent.ToolArrSearchMissing
			r.Outcome = agent.DiagnosisOutcome{Kind: agent.DiagnosisAwaitDownload, Target: &target}
		},
		agent.DiagnosisNeedsApproval: func(r *agent.DiagnosticResult) {
			r.RequiresApproval, r.EscalateAction = true, agent.EscalateRemoveAndSearch
			r.EscalateParams = map[string]any{"name": "x"}
			r.Outcome = agent.DiagnosisOutcome{
				Kind: agent.DiagnosisNeedsApproval, Plan: agent.EscalateRemoveAndSearch,
			}
		},
		agent.DiagnosisManual: func(r *agent.DiagnosticResult) {
			r.RequiresApproval, r.EscalateAction = true, agent.EscalateManualInvestigation
			r.Outcome = agent.DiagnosisOutcome{Kind: agent.DiagnosisManual}
		},
	}
	other := map[agent.DiagnosisKind]lifecycle.State{
		agent.DiagnosisFixNow:         lifecycle.AgentFixed,
		agent.DiagnosisDeferredVerify: lifecycle.Verifying,
		agent.DiagnosisAwaitDownload:  lifecycle.AwaitingDownload,
		agent.DiagnosisNeedsApproval:  lifecycle.ManualTestNeeded,
		agent.DiagnosisManual:         lifecycle.ManualTestNeeded,
	}
	for kind, shape := range outcomes {
		for _, what := range []string{agent.WhatStuckRead, "cant_play"} {
			t.Run(string(kind)+"/"+what, func(t *testing.T) {
				t.Parallel()
				fx, _, inc := routeFixtureWhat(t, what)
				r := fixNowFinding()
				shape(r)
				if err := agent.CheckDiagnosis(r); err != nil {
					t.Fatal(err)
				}
				fx.svc.RouteExitForTest(context.Background(), inc, agent.RunExit{
					State: agent.RunConcluded, Finding: r, Started: true,
				})
				assertRouted(t, kind, what, routeSnap(t, fx, inc.ID), other[kind])
			})
		}
	}
}

func TestRouteExit_UnclassifiedFindingGoesToOwner(t *testing.T) {
	t.Parallel()
	fx, notif, inc := routeFixture(t)
	r := fixNowFinding()
	r.Outcome = agent.DiagnosisOutcome{}
	fx.svc.RouteExitForTest(
		context.Background(),
		inc,
		agent.RunExit{State: agent.RunConcluded, Finding: r, Started: true},
	)
	s := routeSnap(t, fx, inc.ID)
	if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonAgentError || len(notif.msgs) == 0 {
		t.Errorf("unclassified finding → %s/%q owner=%v, want Escalate{agent_error}", s.Status, s.Reason, notif.msgs)
	}
}

func TestRouteExit_NeedsOwnerFromKindNotFlag(t *testing.T) {
	t.Parallel()
	fx, _, inc := routeFixture(t)
	r := fixNowFinding()
	r.Outcome = agent.DiagnosisOutcome{Kind: agent.DiagnosisManual, Reason: "x"}
	fx.svc.RouteExitForTest(
		context.Background(),
		inc,
		agent.RunExit{State: agent.RunConcluded, Finding: r, Started: true},
	)
	s := routeSnap(t, fx, inc.ID)
	if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonNeedsOwner {
		t.Errorf("Manual kind with RA unset → %s/%q, want the owner path", s.Status, s.Reason)
	}
}

func TestA7_7_ServiceTracksThisRunsSearch(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		runResult: &agent.DiagnosticResult{
			RootCause: "missing", Confidence: "high", PrimaryAction: agent.ToolArrSearchMissing, PrimaryReason: "t",
		},
		searchParams: map[string]any{
			"media_type": "tv", "title": "Severance", "season": float64(1), "episode": float64(2),
		},
		search: &agent.SearchTarget{MediaType: "tv", Title: "Severance", Season: 1, Episode: 5},
	}
	svc, database, _ := newPendingOutcomeTestService(t, ag)
	ag.database = database
	inc, err := svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "Severance",
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "awaiting download", func() bool {
		s, serr := database.GetIncidentSnapshot(t.Context(), inc.ID)
		return serr == nil && s.Status == lifecycle.AwaitingDownload
	})
	po, err := database.GetPendingOutcome(t.Context(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if po.Title != "Severance" || po.Season != 1 || po.Episode != 5 {
		t.Errorf("tracked %s S%02dE%02d, want this run's S01E05", po.Title, po.Season, po.Episode)
	}
}

func assertRouted(t *testing.T, kind agent.DiagnosisKind, what string, s lifecycle.Snapshot, want lifecycle.State) {
	t.Helper()
	switch {
	case kind == agent.DiagnosisNeedsApproval || kind == agent.DiagnosisManual:
		if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonNeedsOwner {
			t.Errorf("%s/%s → %s/%q, want the owner path", kind, what, s.Status, s.Reason)
		}
	case what == agent.WhatStuckRead:
		if s.Status != lifecycle.Verifying || s.Verify == nil || s.Verify.Spec.Kind != lifecycle.VerifyStuckRead {
			t.Errorf("%s/stuck_read → %s %+v, want the stuck-read verify", kind, s.Status, s.Verify)
		}
	case s.Status != want:
		t.Errorf("%s → %s, want %s", kind, s.Status, want)
	case kind == agent.DiagnosisAwaitDownload && (s.Download == nil || s.Download.Episode != 5):
		t.Errorf("download target %+v, want the outcome's S01E05", s.Download)
	}
}
