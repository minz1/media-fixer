package lifecycletest

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

const (
	seedMaxActions = 3
	seedThreshold  = 5
	seedChecks     = 3
	seedFirstCap   = 10 * time.Minute
	seedGap        = 5 * time.Minute
	seedStuckFloor = 3 * time.Minute
	seedCheckEvery = 15 * time.Minute
	seedGrace      = 30 * time.Minute
	seedTrackCap   = 12 * time.Hour
	seedKeepSpan   = 72 * time.Hour
	targetEpisode  = 2
)

func Limits() lifecycle.Limits {
	return lifecycle.Limits{
		MaxActions: seedMaxActions, SystemicThreshold: seedThreshold, VerifyChecks: seedChecks,
		ExtensionsMax: seedChecks, Minute: time.Minute, FirstVerifyCap: seedFirstCap, VerifyGap: seedGap,
		StuckReadFloor: seedStuckFloor, ApprovedVerify: seedGap, CheckEvery: seedCheckEvery, NoReleaseGrace: seedGrace,
		Stall: time.Hour, TrackCap: seedTrackCap, KeepSearchingCheck: time.Hour, KeepSearchingSpan: seedKeepSpan,
		PlanTTL: time.Hour,
	}
}

func Escalatable() lifecycle.Finding {
	return lifecycle.Finding{
		EscalateAction: lifecycle.EscalateRemoveAndSearch,
		JSON:           json.RawMessage(`{"escalate_action":"remove_and_search","root_cause":"bad file"}`),
	}
}

func Target() *lifecycle.Download {
	return &lifecycle.Download{MediaType: "series", Title: "Show", Season: 1, Episode: targetEpisode}
}

type Spec struct {
	Title   string
	What    string
	State   lifecycle.State
	Reason  lifecycle.Reason
	Finding lifecycle.Finding
	Plan    []byte
	Locked  bool
}

func Report(title, what string) *db.NewIncident {
	if what == "" {
		what = "cant_play"
	}
	return &db.NewIncident{
		Source: "discord", ReportedBy: "someone", ReporterDiscordID: "u-1", What: what, Title: title,
	}
}

func Decide(ev lifecycle.Event, l lifecycle.Limits, now time.Time) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		cur.In.Limits = l
		d, err := lifecycle.Next(cur, ev, now)
		return db.Decision{Decision: d}, err
	}
}

type seeder struct {
	t   testing.TB
	d   *db.DB
	s   lifecycle.Snapshot
	now time.Time
}

func (sd *seeder) step(ev lifecycle.Event) {
	sd.t.Helper()
	if ev.Gen == 0 {
		ev.Gen = sd.s.Generation
	}
	ev.Version, ev.HasVersion = sd.s.Version, true
	a, err := sd.d.ApplyIncident(sd.t.Context(), sd.s.ID, Decide(ev, Limits(), sd.now))
	if err != nil {
		sd.t.Fatalf("seed %s: %s: %v", sd.s.Status, ev.Kind, err)
	}
	sd.s = a.Snap
}

func (sd *seeder) finish(o lifecycle.Outcome) {
	sd.step(lifecycle.Event{Kind: lifecycle.RunFinished, Outcome: o})
}

func Seed(t testing.TB, d *db.DB, sp Spec) lifecycle.Snapshot {
	t.Helper()
	return SeedReport(t, d, Report(sp.Title, sp.What), sp)
}

func SeedReport(t testing.TB, d *db.DB, r *db.NewIncident, sp Spec) lifecycle.Snapshot {
	t.Helper()
	l := Limits()
	l.SystemicThreshold = math.MaxInt32
	if sp.State == lifecycle.Blocked {
		l.SystemicThreshold = 0
	}
	now := time.Now()
	a, err := d.CreateFromReport(
		t.Context(),
		r,
		Decide(lifecycle.Event{Kind: lifecycle.Reported}, l, now),
	)
	if err != nil {
		t.Fatalf("seed report: %v", err)
	}
	if a.Merged {
		t.Fatalf("seed report %q merged into %s", r.Title, a.Snap.ID)
	}
	sd := &seeder{t: t, d: d, s: a.Snap, now: now}
	sd.drive(sp)
	if sp.Locked && !sd.s.Locked {
		sd.force(func(s *lifecycle.Snapshot) { s.Locked = true })
	}
	return sd.s
}

func (sd *seeder) force(edit func(s *lifecycle.Snapshot)) {
	sd.t.Helper()
	a, err := sd.d.ApplyIncident(sd.t.Context(), sd.s.ID, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		edit(&n)
		return db.Decision{Next: n}, nil
	})
	if err != nil {
		sd.t.Fatalf("seed force: %v", err)
	}
	sd.s = a.Snap
}

func (sd *seeder) drive(sp Spec) {
	switch sp.State {
	case lifecycle.Open, lifecycle.Blocked:
		return
	case lifecycle.Reopened:
		sd.step(lifecycle.Event{Kind: lifecycle.OwnerRerun})
		return
	case lifecycle.Resolved:
		sd.step(lifecycle.Event{Kind: lifecycle.OwnerResolve})
		return
	case lifecycle.Investigating, lifecycle.Verifying, lifecycle.AwaitingDownload, lifecycle.ManualTestNeeded,
		lifecycle.ExecutingPlan, lifecycle.AgentFixed, lifecycle.None:
	}
	sd.step(lifecycle.Event{Kind: lifecycle.RunClaimed})
	if len(sp.Finding.JSON) > 0 || sp.State == lifecycle.ExecutingPlan {
		f := sp.Finding
		if len(f.JSON) == 0 {
			f = Escalatable()
		}
		sd.step(lifecycle.Event{Kind: lifecycle.AttemptFinished, Finding: &f})
	}
	sd.conclude(sp)
}

func (sd *seeder) conclude(sp Spec) {
	switch sp.State {
	case lifecycle.Verifying:
		spec := lifecycle.VerifySpec{Kind: lifecycle.VerifyGeneric}
		if sp.What == lifecycle.WhatStuckRead {
			spec.Kind = lifecycle.VerifyStuckRead
		}
		sd.finish(lifecycle.Outcome{Kind: lifecycle.OutcomeVerify, Spec: spec})
	case lifecycle.AwaitingDownload:
		sd.finish(lifecycle.Outcome{Kind: lifecycle.OutcomeAwaitDownload, Target: Target()})
	case lifecycle.AgentFixed:
		sd.finish(lifecycle.Outcome{Kind: lifecycle.OutcomeFixed})
	case lifecycle.ManualTestNeeded:
		sd.toOwner(sp)
	case lifecycle.ExecutingPlan:
		sd.toOwner(Spec{Reason: lifecycle.ReasonNeedsOwner, Plan: planOr(sp.Plan)})
		sd.force(func(n *lifecycle.Snapshot) { shape(n, lifecycle.ExecutingPlan, Limits()) })
	case lifecycle.Open, lifecycle.Reopened, lifecycle.Investigating, lifecycle.Blocked, lifecycle.Resolved,
		lifecycle.None:
	}
}

func planOr(p []byte) []byte {
	if len(p) == 0 {
		return []byte(`{"files":[]}`)
	}
	return p
}

func (sd *seeder) toOwner(sp Spec) {
	reason := sp.Reason
	if reason == lifecycle.ReasonNone {
		reason = lifecycle.ReasonNeedsOwner
	}
	if reason == lifecycle.ReasonNoRelease {
		sd.finish(lifecycle.Outcome{Kind: lifecycle.OutcomeAwaitDownload, Target: Target()})
		sd.now = sd.now.Add(Limits().NoReleaseGrace + time.Minute)
		sd.step(lifecycle.Event{Kind: lifecycle.DownloadObserved, Obs: &lifecycle.Observation{}})
		return
	}
	sd.finish(lifecycle.Outcome{Kind: lifecycle.OutcomeEscalate, Reason: reason})
	if len(sp.Plan) > 0 {
		sd.step(lifecycle.Event{Kind: lifecycle.OwnerPreview, Plan: sp.Plan, PlanFP: card.Fingerprint(sp.Plan)})
	}
}
