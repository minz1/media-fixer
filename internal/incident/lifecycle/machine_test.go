package lifecycle_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/operation"
)

func obligations(before lifecycle.Snapshot, t tev) error {
	cur, _ := prepare(before, t)
	d, err := apply(before, t)
	if err == nil {
		return decisionObligations(before, cur, t, d)
	}
	return nil
}

func effectObligations(cur lifecycle.Snapshot, d lifecycle.Decision) error {
	n := d.Next
	for _, e := range d.Effects {
		switch e.Kind {
		case lifecycle.LaunchRun, lifecycle.CancelRun, lifecycle.ScheduleCheck, lifecycle.ExecutePlan:
			if e.Gen != n.Generation {
				return fmt.Errorf("%s carries gen %d, snapshot has %d", e.Kind, e.Gen, n.Generation)
			}
		case lifecycle.NotifyReporters, lifecycle.NotifyOwner, lifecycle.SeerrResolve, lifecycle.SeerrComment:
		}
		if e.Kind == lifecycle.LaunchRun && (cur.In.Paused || n.Parked) {
			return errors.New("D4: launched while paused")
		}
		if e.Kind == lifecycle.LaunchRun && n.Status != lifecycle.Open && n.Status != lifecycle.Reopened {
			return fmt.Errorf("launched into %s", n.Status)
		}
	}
	return nil
}

func decisionObligations(before, cur lifecycle.Snapshot, t tev, d lifecycle.Decision) error {
	n := d.Next
	if err := effectObligations(cur, d); err != nil {
		return err
	}
	if err := opObligations(t, d); err != nil {
		return err
	}
	if d.JournalOnly && (n.Status != before.Status || n.Generation != before.Generation) {
		return errors.New("journal-only decision changed the row")
	}
	if t.ev.Kind == lifecycle.OwnerRerun && (n.ActionCount != 0 || n.Locked) {
		return errors.New("D1: owner rerun kept the budget")
	}
	system := []lifecycle.EventKind{lifecycle.SystemRerun, lifecycle.StaleRun, lifecycle.Relaunch, lifecycle.Resumed}
	if slices.Contains(system, t.ev.Kind) && (n.ActionCount != before.ActionCount || n.Locked != before.Locked) {
		return errors.New("D1: system event reset the budget")
	}
	if n.Generation < before.Generation || n.Version < before.Version {
		return errors.New("a counter went backwards")
	}
	return nil
}

func opObligations(t tev, d lifecycle.Decision) error {
	n := d.Next
	if err := lifecycle.CheckDecision(d); err != nil {
		return err
	}
	halfRun := n.Status == lifecycle.ManualTestNeeded &&
		(n.Reason == lifecycle.ReasonPlanPartial || n.Reason == lifecycle.ReasonPlanInterrupted)
	if halfRun && n.Op != nil && n.Op.Status != operation.FailedAtStep {
		return errors.New("half-run plan with an op that did not fail")
	}
	if t.ev.Kind == lifecycle.OwnerResumeOp && n.Status == lifecycle.ExecutingPlan && n.Plan == nil {
		return errors.New("I12: resumed into executing_plan without a plan")
	}
	for _, e := range d.Effects {
		if e.Kind == lifecycle.ExecutePlan && n.Op != nil && e.OpID != n.Op.ID {
			return fmt.Errorf("execute_plan carries op %d, snapshot has %d", e.OpID, n.Op.ID)
		}
	}
	return nil
}

func TestIncidentMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, incidentMachine())
}

func TestIncidentMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, incidentMachine())
}

func TestIncidentMachine_Invariants(t *testing.T) {
	t.Parallel()
	m := incidentMachine()
	for _, s := range lifecycle.States() {
		for _, sn := range samples(s) {
			if err := lifecycle.Check(sn); err != nil {
				t.Errorf("sample %s is illegal: %v", key(sn), err)
			}
		}
	}
	machinetest.Invariants(t, m)
}

func FuzzIncidentMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{0, 20, 40, 60, 80, 100, 120, 140, 160, 180, 200})
	f.Add([]byte{0, 5, 5, 9, 9, 9, 33, 77, 254, 3, 1})
	m := incidentMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}

func TestVersionChecked_MatchesRuling(t *testing.T) {
	t.Parallel()
	checked := []lifecycle.EventKind{
		lifecycle.OwnerApprove, lifecycle.OwnerPreview, lifecycle.OwnerKeepSearching, lifecycle.OwnerResumeOp,
	}
	for _, k := range lifecycle.EventKinds() {
		if got, want := lifecycle.VersionChecked(k), slices.Contains(checked, k); got != want {
			t.Errorf("VersionChecked(%s) = %v, want %v", k, got, want)
		}
	}
}

func actionEvent(k lifecycle.ActionKind) lifecycle.Event {
	ev := lifecycle.Event{Kind: k.Event(), Plan: []byte(`{}`), PlanFP: "fp1"}
	if k == lifecycle.ActApprove {
		spec := opSpec()
		ev.Spec = &spec
	}
	return ev
}

func TestIncidentMachine_ActionsAreAccepted(t *testing.T) {
	t.Parallel()
	for _, s := range lifecycle.States() {
		for _, sn := range samples(s) {
			if sn.In.Paused {
				continue
			}
			for _, k := range lifecycle.ActionKinds() {
				ev := actionEvent(k)
				_, err := apply(sn, tev{ev: ev})
				offered := lifecycle.Offers(sn, base(), k)
				if k == lifecycle.ActPreview || k == lifecycle.ActRefresh {
					offered = lifecycle.Offers(sn, base(), lifecycle.ActPreview) ||
						lifecycle.Offers(sn, base(), lifecycle.ActRefresh)
				}
				if offered != (err == nil) {
					t.Errorf("%s: action %s offered=%v but Next returned %v", key(sn), k, offered, err)
				}
			}
		}
	}
}

func TestIncidentMachine_VersionBumpsOnEveryOwnerVisibleChange(t *testing.T) {
	t.Parallel()
	b := snap(lifecycle.ManualTestNeeded)
	changes := map[string]func(*lifecycle.Snapshot){
		"status":  func(s *lifecycle.Snapshot) { s.Status = lifecycle.Resolved },
		"reason":  func(s *lifecycle.Snapshot) { s.Reason = lifecycle.ReasonAgentError },
		"lock":    func(s *lifecycle.Snapshot) { s.Locked = true },
		"budget":  func(s *lifecycle.Snapshot) { s.ActionCount++ },
		"parked":  func(s *lifecycle.Snapshot) { s.Parked = true },
		"finding": finding(escalatable()),
		"plan":    planAt(base()),
		"plan fp": func(s *lifecycle.Snapshot) { s.Plan = &lifecycle.Plan{At: base(), FP: "fp2"} },
		"stage": func(s *lifecycle.Snapshot) {
			s.Download = download(lifecycle.StageDownloading, true, false)
		},
		"queue status": func(s *lifecycle.Snapshot) {
			s.Download = download(lifecycle.StageSearching, false, false)
			s.Download.QueueStatus = "queued"
		},
		"keep searching": func(s *lifecycle.Snapshot) { s.Download = download(lifecycle.StageSearching, false, true) },
		"target": func(s *lifecycle.Snapshot) {
			s.Download = download(lifecycle.StageSearching, false, false)
			s.Download.Episode = 9
		},
		"op id":      func(s *lifecycle.Snapshot) { s.Op.ID++ },
		"op version": func(s *lifecycle.Snapshot) { s.Op.Version++ },
		"op status":  func(s *lifecycle.Snapshot) { s.Op.Status = operation.FailedAtStep },
		"op set":     setOp(runningOp()),
	}
	from := with(b, planAt(base()), dl(download(lifecycle.StageSearching, false, false)))
	withOp := with(b, setOp(runningOp()))
	for name, change := range changes {
		cur := from
		if name == "plan" || name == "stage" || name == "keep searching" || name == "queue status" ||
			name == "target" || name == "op set" {
			cur = b
		}
		if strings.HasPrefix(name, "op ") && name != "op set" {
			cur = withOp
		}
		next := with(cur.Clone(), change)
		if got := lifecycle.Versioned(cur, next).Version; got != cur.Version+1 {
			t.Errorf("%s: version %d, want %d", name, got, cur.Version+1)
		}
	}
}

func TestIncidentMachine_PureRescheduleKeepsVersion(t *testing.T) {
	t.Parallel()
	cur := with(snap(lifecycle.Verifying))
	next := cur.Clone()
	next.NextCheck = base().Add(time.Hour)
	next.Verify.ChecksLeft = 1
	next.Verify.Spec.Extensions = 1
	if got := lifecycle.Versioned(cur, next).Version; got != cur.Version {
		t.Errorf("reschedule bumped version to %d", got)
	}
	dcur := snap(lifecycle.AwaitingDownload)
	dnext := dcur.Clone()
	dnext.Download.LastProgressPct = 40
	dnext.Download.LastProgressAt = base().Add(time.Minute)
	dnext.NextCheck = base().Add(time.Hour)
	if got := lifecycle.Versioned(dcur, dnext).Version; got != dcur.Version {
		t.Errorf("progress bumped version to %d", got)
	}
	for _, ev := range []tev{
		{ev: lifecycle.Event{Kind: lifecycle.VerifyObserved, Verify: lifecycle.VerifyNotYet}},
		{ev: lifecycle.Event{Kind: lifecycle.VerifyObserved, Verify: lifecycle.VerifyUnknown}},
	} {
		d, err := apply(cur, ev)
		if err != nil || d.Next.Version != cur.Version {
			t.Errorf("%v: version %d err %v", ev, d.Next.Version, err)
		}
	}
}

func TestIncidentMachine_StaleAttemptFinishedIsJournalOnly(t *testing.T) {
	t.Parallel()
	esc := escalatable()
	for _, s := range lifecycle.States() {
		for _, stale := range []bool{false, true} {
			cur := snap(s)
			d, err := apply(cur, tev{ev: lifecycle.Event{Kind: lifecycle.AttemptFinished, Finding: &esc}, stale: stale})
			if err != nil {
				t.Fatalf("%s stale=%v: %v", s, stale, err)
			}
			wantWrite := s == lifecycle.Investigating && !stale
			if d.JournalOnly == wantWrite {
				t.Errorf("%s stale=%v: journal only %v", s, stale, d.JournalOnly)
			}
			if !wantWrite && string(d.Next.Finding.JSON) != string(cur.Finding.JSON) {
				t.Errorf("%s stale=%v: finding written", s, stale)
			}
		}
	}
}

func TestDownload_StageNeverFromQueueStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []string{
		"", "searching", "queued", "downloading", "completed", "importPending", "importBlocked", "paused",
		"delay", "failed", "warning",
	} {
		for _, inQueue := range []bool{true, false} {
			cur := with(snap(lifecycle.AwaitingDownload), dl(download(lifecycle.StageDownloading, true, false)))
			obs := &lifecycle.Observation{InQueue: inQueue, QueueStatus: status, ProgressPct: 50}
			d, err := apply(cur, tev{ev: lifecycle.Event{Kind: lifecycle.DownloadObserved, Obs: obs}})
			if err != nil {
				t.Fatalf("%q: %v", status, err)
			}
			want := lifecycle.StageImporting
			if inQueue {
				want = lifecycle.StageDownloading
			}
			if d.Next.Download.Stage != want {
				t.Errorf("queue %q in=%v: stage %s, want %s", status, inQueue, d.Next.Download.Stage, want)
			}
			if inQueue && d.Next.Download.QueueStatus != status {
				t.Errorf("queue status %q not kept: %q", status, d.Next.Download.QueueStatus)
			}
		}
	}
}
