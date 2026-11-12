package incident_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
)

func TestE4_CompletionWakesOnlyScanWaiting(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	waiting := env.verifying(t, "Scan waiting", "cant_play")
	waiting = lifecycletest.Force(t, env.db, waiting.ID, func(n *lifecycle.Snapshot) {
		n.Verify.ChecksLeft = 1
		n.Verify.Spec.ETANoted = true
		n.Verify.Spec.Extensions = 1
	})
	early := env.verifying(t, "Early check", "cant_play")
	stuck := env.verifying(t, "Stuck read", lifecycle.WhatStuckRead)
	lifecycletest.Force(t, env.db, stuck.ID, func(n *lifecycle.Snapshot) { n.Verify.Spec.ETANoted = true })

	if err := env.svc.ArmScanSettled(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	at, ok := env.svc.ArmedAtForTest(waiting.ID)
	if !ok {
		t.Fatal("the scan-waiting verify was not armed")
	}
	if at.After(time.Now().Add(time.Second)) {
		t.Fatalf("armed at %v, want now", at)
	}
	if _, armed := env.svc.ArmedAtForTest(early.ID); armed {
		t.Fatal("a verify not waiting on a scan was armed")
	}
	if _, armed := env.svc.ArmedAtForTest(stuck.ID); armed {
		t.Fatal("a stuck-read verify was armed")
	}
	if got := env.snap(t, waiting.ID); got.Generation != waiting.Generation {
		t.Fatalf("generation moved %d -> %d on arming", waiting.Generation, got.Generation)
	}
}

func TestE4_ScanFinishedSettlesDisruption(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, newObservingAgent())
	jrnl := journal.New(env.db)
	svc := incident.NewService(t.Context(), env.db, jrnl, newObservingAgent(), nil, nil, env.notif,
		slog.New(slog.DiscardHandler))
	t.Cleanup(func() { stopService(t, svc) })
	inc := env.verifying(t, "Scanned show", "cant_play")
	ctx := t.Context()
	scanAt := time.Now().Add(-5 * time.Minute)
	logScan := func(callID string, at time.Time) {
		t.Helper()
		if err := jrnl.LogActionGen(ctx, &db.ActionLog{
			IncidentID: inc.ID, Action: "jellyfin_library_scan", Status: db.ActionApplied,
			TriggeredBy: "agent", AppliedAt: &at,
		}, true, inc.Generation, callID); err != nil {
			t.Fatal(err)
		}
	}
	logScan("scan-1", scanAt)

	if err := svc.ArmScanSettled(ctx, scanAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, settled := jrnl.DisruptionSettledAt(ctx, "scan-1"); settled {
		t.Fatal("a scan that ended before ours was triggered settled it")
	}
	if err := svc.ArmScanSettled(ctx, scanAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, settled := jrnl.DisruptionSettledAt(ctx, "scan-1"); !settled {
		t.Fatal("the scan-finished event did not settle the scan disruption")
	}
	if err := svc.ArmScanSettled(ctx, scanAt.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := countSettled(ctx, t, env.db); n != 1 {
		t.Fatalf("disruption_settled records %d, want 1", n)
	}

	restartAt := time.Now().Add(-time.Minute)
	if err := jrnl.LogActionGen(ctx, &db.ActionLog{
		IncidentID: inc.ID, Action: "restart_jellyfin", Status: db.ActionApplied, TriggeredBy: "agent",
		AppliedAt: &restartAt,
	}, true, inc.Generation, "restart-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ArmScanSettled(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, settled := jrnl.DisruptionSettledAt(ctx, "restart-1"); settled {
		t.Fatal("a scan-finished event settled a restart")
	}
}

func countSettled(ctx context.Context, t *testing.T, database *db.DB) int {
	t.Helper()
	events, err := database.EventsByKind(ctx, string(journal.KindDisruptionSettled), 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}
