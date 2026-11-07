package incident_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/journal"
)

func shutdownNow(t *testing.T, svc *incident.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), notifyWaitTimeout)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestA10_10_ApproveDuringShutdownIsRefused(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.runBlocks = true
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)
	if _, err := fx.svc.Handle(context.Background(), &incident.Report{
		Source: "discord", ReportedBy: "bob", What: "cant_play", Title: "Holder",
	}); err != nil {
		t.Fatal(err)
	}
	<-ag.runStart

	done := make(chan error, 1)
	go func() {
		done <- fx.svc.ApproveEscalation(context.Background(), inc.ID, fp, fx.svc.CurrentVersion(context.Background(), inc.ID))
	}()
	time.Sleep(escalationSettle)
	shutdownNow(t, fx.svc)

	if err := <-done; !errors.Is(err, card.ErrRestarting) {
		t.Fatalf("Approve after the slot freed during shutdown = %v, want ErrRestarting", err)
	}
	if n := ag.execCalls.Load(); n != 0 {
		t.Errorf("ExecuteApprovedPlan ran %d times during shutdown", n)
	}
	if _, _, err := fx.db.GetEscalationPlan(context.Background(), inc.ID); err != nil {
		t.Errorf("the refused approve consumed the plan: %v", err)
	}
}

func TestShutdown_WaitsForInFlightApprove(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)

	approved := make(chan error, 1)
	go func() {
		approved <- fx.svc.ApproveEscalation(context.Background(), inc.ID, fp, fx.svc.CurrentVersion(context.Background(), inc.ID))
	}()
	<-ag.execStart
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyWaitTimeout)
		defer cancel()
		stopped <- fx.svc.Shutdown(ctx)
	}()
	select {
	case err := <-stopped:
		t.Fatalf("Shutdown returned %v while the approved plan was still executing", err)
	case <-time.After(escalationSettle):
	}
	close(ag.execGate)
	if err := <-approved; err != nil {
		t.Errorf("Approve: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

type ctxRecordingAgent struct {
	*escalationAgent

	mu     sync.Mutex
	execOK bool
}

func (a *ctxRecordingAgent) ExecuteApprovedPlan(
	ctx context.Context, result *agent.DiagnosticResult, plan []byte,
) (any, error) {
	out, err := a.escalationAgent.ExecuteApprovedPlan(ctx, result, plan)
	a.mu.Lock()
	a.execOK = ctx.Err() == nil
	a.mu.Unlock()
	return out, err
}

func TestShutdown_DoesNotCancelApprovedPlanCtx(t *testing.T) {
	t.Parallel()
	ag := &ctxRecordingAgent{escalationAgent: newEscalationAgent()}
	ag.execGate = make(chan struct{})
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)

	approved := make(chan error, 1)
	go func() {
		approved <- fx.svc.ApproveEscalation(context.Background(), inc.ID, fp, fx.svc.CurrentVersion(context.Background(), inc.ID))
	}()
	<-ag.execStart
	stopped := make(chan struct{})
	go func() {
		shutdownNow(t, fx.svc)
		close(stopped)
	}()
	time.Sleep(escalationSettle)
	close(ag.execGate)
	<-approved
	<-stopped
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if !ag.execOK {
		t.Error("the approved plan's context was cancelled by Shutdown")
	}
}

func ownerSnapshot(t *testing.T, fx *escalationFixture, id string) string {
	t.Helper()
	ctx := context.Background()
	inc, err := fx.db.GetIncident(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := json.Marshal(inc)
	plan, at, _ := fx.db.GetEscalationPlan(ctx, id)
	events, err := fx.db.EventsSince(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	paused, _ := fx.db.GetSetting(ctx, "autonomous_paused")
	po, _ := fx.db.GetPendingOutcome(ctx, id)
	pending, _ := json.Marshal(po)
	return fmt.Sprintf("%s\n%s %s\n%d events\npaused=%s\n%s", row, plan, at, len(events), paused, pending)
}

func TestA10_10_OwnerActionsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	fx := newEscalationFixture(t, newEscalationAgent())
	ctx := context.Background()
	inc := fx.incident(t)
	fx.preview(t, inc.ID)
	if err := fx.db.SetAutonomousLocked(ctx, inc.ID, true); err != nil {
		t.Fatal(err)
	}
	po := &db.PendingOutcome{MediaType: "tv", Title: "The Boys", Season: 1, Episode: 1, StartedAt: time.Now()}
	if err := fx.db.SetPendingOutcome(ctx, inc.ID, po, time.Now()); err != nil {
		t.Fatal(err)
	}
	shutdownNow(t, fx.svc)
	before := ownerSnapshot(t, fx, inc.ID)

	actions := []struct {
		name string
		do   func() error
	}{
		{"Rerun", func() error { return fx.svc.Rerun(ctx, inc.ID, incident.Version{}) }},
		{"Resolve", func() error { return fx.svc.Resolve(ctx, inc.ID, incident.Version{}) }},
		{"Unlock", func() error { return fx.svc.Unlock(ctx, inc.ID, incident.Version{}) }},
		{"KeepSearching", func() error { return fx.svc.KeepSearching(ctx, inc.ID, incident.Version{}) }},
		{"PreviewEscalation", func() error {
			_, _, err := fx.svc.PreviewEscalation(ctx, inc.ID, fx.svc.CurrentVersion(ctx, inc.ID))
			return err
		}},
		{"SetAutonomousPaused", func() error { return fx.svc.SetAutonomousPaused(ctx, true) }},
	}
	for _, a := range actions {
		if err := a.do(); !errors.Is(err, card.ErrRestarting) {
			t.Errorf("%s during shutdown = %v, want ErrRestarting", a.name, err)
		}
		if after := ownerSnapshot(t, fx, inc.ID); after != before {
			t.Errorf("%s wrote during shutdown:\nbefore %s\nafter  %s", a.name, before, after)
		}
	}
}

func TestRecovery_RefusedRerunLogsInfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		age  time.Duration
		call func(*incident.Service)
	}{
		{"recovery", 0, func(s *incident.Service) { s.RecoverZombies(context.Background()) }},
		{"stale sweep", staleAge, func(s *incident.Service) { s.SweepStaleRuns(context.Background()) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			database, inc := newReopenedIncident(t, c.age)
			var buf bytes.Buffer
			var mu sync.Mutex
			log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
			svc := incident.NewService(
				context.Background(), database, journal.New(database), newAlwaysFixedAgent(), nil, nil,
				newSyncNotifier(), log)
			shutdownNow(t, svc)
			events, err := database.EventsSince(context.Background(), inc.ID, 0)
			if err != nil {
				t.Fatal(err)
			}

			c.call(svc)

			after, err := database.EventsSince(context.Background(), inc.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(events) {
				t.Errorf("the refused rerun appended %d events", len(after)-len(events))
			}
			mu.Lock()
			defer mu.Unlock()
			out := buf.String()
			if strings.Contains(out, "level=ERROR") {
				t.Errorf("a rerun refused during shutdown logged at ERROR:\n%s", out)
			}
			if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "media-fixer is restarting") {
				t.Errorf("no INFO line for the refused rerun:\n%s", out)
			}
		})
	}
}
