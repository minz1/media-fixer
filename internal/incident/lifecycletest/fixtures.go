package lifecycletest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func Incident(t testing.TB, d *db.DB, inc *db.Incident) *db.Incident {
	t.Helper()
	st := inc.Status
	if st == lifecycle.None {
		st = lifecycle.Open
	}
	r := &db.NewIncident{
		ID: inc.ID, Source: inc.Source, ReportedBy: inc.ReportedBy, What: inc.What, Title: inc.Title,
		JellyfinItemID: inc.JellyfinItemID, Details: inc.Details,
	}
	if r.What == "" {
		r.What = "cant_play"
	}
	l := Limits()
	a, err := d.CreateFromReport(t.Context(), r, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur
		n.Status, n.Generation = lifecycle.Open, 1
		shape(&n, st, l)
		return db.Decision{Next: n}, nil
	})
	if err != nil {
		t.Fatalf("fixture incident: %v", err)
	}
	if a.Merged {
		t.Fatalf("fixture incident %q merged into %s", r.Title, a.Snap.ID)
	}
	got, err := d.GetIncident(t.Context(), a.Snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	*inc = *got
	return inc
}

func Force(t testing.TB, d *db.DB, id string, edit func(s *lifecycle.Snapshot)) lifecycle.Snapshot {
	t.Helper()
	a, err := d.ApplyIncident(t.Context(), id, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		n.In.Limits = Limits()
		edit(&n)
		return db.Decision{Next: n}, nil
	})
	if err != nil {
		t.Fatalf("fixture write: %v", err)
	}
	return a.Snap
}

func MoveTo(t testing.TB, d *db.DB, id string, st lifecycle.State) lifecycle.Snapshot {
	t.Helper()
	return Force(t, d, id, func(n *lifecycle.Snapshot) { shape(n, st, Limits()) })
}

func Lock(t testing.TB, d *db.DB, id string, locked bool) {
	t.Helper()
	Force(t, d, id, func(n *lifecycle.Snapshot) { n.Locked = locked || n.Status == lifecycle.Blocked })
}

func Spend(t testing.TB, d *db.DB, id string, n int) {
	t.Helper()
	Force(t, d, id, func(s *lifecycle.Snapshot) { s.ActionCount += n })
}

func Finding(t testing.TB, d *db.DB, id string, finding any) {
	t.Helper()
	b, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		EscalateAction string `json:"escalate_action"`
	}
	_ = json.Unmarshal(b, &f)
	Force(t, d, id, func(n *lifecycle.Snapshot) {
		n.Finding = lifecycle.Finding{EscalateAction: f.EscalateAction, JSON: b}
	})
}

func StorePlan(t testing.TB, d *db.DB, id string, plan []byte) {
	t.Helper()
	Force(t, d, id, func(n *lifecycle.Snapshot) {
		if n.Status != lifecycle.ExecutingPlan {
			shape(n, lifecycle.ManualTestNeeded, Limits())
		}
		n.Plan = &lifecycle.Plan{JSON: append([]byte(nil), plan...), At: time.Now(), FP: card.Fingerprint(plan)}
	})
}

func Track(t testing.TB, d *db.DB, id string, po *db.PendingOutcome, next time.Time) {
	t.Helper()
	if _, err := d.ApplyIncident(t.Context(), id, TrackDecide(po, next)); err != nil {
		t.Fatalf("fixture track: %v", err)
	}
}

func TrackDecide(po *db.PendingOutcome, next time.Time) db.Decide {
	dl := &lifecycle.Download{
		MediaType: po.MediaType, Title: po.Title, Season: po.Season, Episode: po.Episode, StartedAt: po.StartedAt,
		Stage: lifecycle.Stage(po.LastStage), QueueStatus: po.QueueStatus, LastProgressAt: po.LastProgressAt,
		LastProgressPct: po.LastProgressPct, GrabNotified: po.GrabNotified, KeepSearching: po.KeepSearching,
		KeepSearchingUntil: po.KeepSearchingUntil,
	}
	if dl.Stage == "" {
		dl.Stage = lifecycle.StageSearching
	}
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		if n.Status != lifecycle.ManualTestNeeded || n.Reason != lifecycle.ReasonNoRelease {
			shape(&n, lifecycle.AwaitingDownload, Limits())
		}
		n.Download = dl
		if n.Status == lifecycle.AwaitingDownload {
			n.NextCheck = next
		}
		return db.Decision{Next: n}, nil
	}
}

func shape(n *lifecycle.Snapshot, st lifecycle.State, l lifecycle.Limits) {
	keepPlan, keepDownload := n.Plan, n.Download
	n.Status = st
	n.Reason, n.Plan, n.Download, n.Verify, n.NextCheck = lifecycle.ReasonNone, nil, nil, nil, time.Time{}
	if st != lifecycle.Open && st != lifecycle.Reopened {
		n.Parked = false
	}
	switch st {
	case lifecycle.Verifying:
		n.NextCheck = time.Now().Add(l.VerifyGap)
		n.Verify = &lifecycle.Verify{ChecksLeft: 1, Spec: lifecycle.VerifySpec{Kind: lifecycle.VerifyGeneric}}
		if n.What == lifecycle.WhatStuckRead {
			n.Verify.Spec.Kind = lifecycle.VerifyStuckRead
		}
	case lifecycle.AwaitingDownload:
		n.NextCheck = time.Now().Add(l.CheckEvery)
		n.Download = keepDownload
		if n.Download == nil {
			n.Download = Target()
			n.Download.Stage, n.Download.StartedAt = lifecycle.StageSearching, time.Now()
		}
	case lifecycle.ManualTestNeeded:
		n.Reason, n.Plan = lifecycle.ReasonNeedsOwner, keepPlan
	case lifecycle.ExecutingPlan:
		n.Plan = keepPlan
		if n.Plan == nil {
			p := planOr(nil)
			n.Plan = &lifecycle.Plan{JSON: p, At: time.Now(), FP: card.Fingerprint(p)}
		}
	case lifecycle.Blocked:
		n.Locked = true
	case lifecycle.Open, lifecycle.Reopened, lifecycle.Investigating, lifecycle.AgentFixed, lifecycle.Resolved,
		lifecycle.None:
	}
}
