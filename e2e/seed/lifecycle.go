package main

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

const seedChecks = 3

func seedLimits() lifecycle.Limits {
	return lifecycle.Limits{
		MaxActions: seedChecks, SystemicThreshold: math.MaxInt32, VerifyChecks: seedChecks, ExtensionsMax: seedChecks,
		Minute: time.Minute, FirstVerifyCap: time.Hour, VerifyGap: time.Hour, StuckReadFloor: time.Minute,
		ApprovedVerify: time.Hour, CheckEvery: time.Hour, NoReleaseGrace: time.Hour, Stall: time.Hour,
		TrackCap: day, KeepSearchingCheck: time.Hour, KeepSearchingSpan: day, PlanTTL: time.Hour,
	}
}

type seeder struct {
	d *db.DB
	l lifecycle.Limits
}

func (s seeder) decide(ev lifecycle.Event) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		cur.In.Limits = s.l
		ev.Gen = cur.Generation
		d, err := lifecycle.Next(cur, ev, time.Now())
		return db.Decision{Decision: d}, err
	}
}

func (s seeder) step(ctx context.Context, id string, ev lifecycle.Event) error {
	_, err := s.d.ApplyIncident(ctx, id, s.decide(ev))
	return err
}

func seedIncident(
	ctx context.Context, d *db.DB, title string, st db.IncidentStatus, finding map[string]any,
) (string, error) {
	s := seeder{d: d, l: seedLimits()}
	if st == db.StatusBlocked {
		s.l.SystemicThreshold = 0
	}
	a, err := d.CreateFromReport(ctx, &db.NewIncident{
		Source: "discord", ReportedBy: "viewer", What: "playback", Title: title, Details: "Reported from the e2e seed.",
	}, s.decide(lifecycle.Event{Kind: lifecycle.Reported}))
	if err != nil || st == db.StatusBlocked || st == db.StatusOpen {
		return a.Snap.ID, err
	}
	id := a.Snap.ID
	if err = s.step(ctx, id, lifecycle.Event{Kind: lifecycle.RunClaimed}); err != nil || st == db.StatusInvestigating {
		return id, err
	}
	b, err := json.Marshal(finding)
	if err != nil {
		return id, err
	}
	if err = s.step(
		ctx,
		id,
		lifecycle.Event{Kind: lifecycle.AttemptFinished, Finding: &lifecycle.Finding{JSON: b}},
	); err != nil {
		return id, err
	}
	return id, s.step(ctx, id, s.conclusion(st))
}

func (s seeder) conclusion(st db.IncidentStatus) lifecycle.Event {
	switch st {
	case lifecycle.Open, lifecycle.Reopened, lifecycle.Investigating, lifecycle.AwaitingDownload,
		lifecycle.ManualTestNeeded, lifecycle.ExecutingPlan, lifecycle.Blocked, lifecycle.None:
	case db.StatusVerifying:
		return lifecycle.Event{Kind: lifecycle.RunFinished, Outcome: lifecycle.Outcome{
			Kind: lifecycle.OutcomeVerify, Spec: lifecycle.VerifySpec{Kind: lifecycle.VerifyGeneric}, After: time.Hour,
		}}
	case db.StatusResolved:
		return lifecycle.Event{Kind: lifecycle.OwnerResolve}
	case db.StatusAgentFixed:
		return lifecycle.Event{Kind: lifecycle.RunFinished, Outcome: lifecycle.Outcome{Kind: lifecycle.OutcomeFixed}}
	}
	return lifecycle.Event{Kind: lifecycle.RunFinished, Outcome: lifecycle.Outcome{
		Kind: lifecycle.OutcomeEscalate, Reason: lifecycle.ReasonNeedsOwner,
	}}
}
