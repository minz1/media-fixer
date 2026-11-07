package incident_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type dlFixture struct {
	svc    *incident.Service
	db     *db.DB
	notif  *syncNotifier
	inc    *db.Incident
	obs    atomic.Pointer[agent.PendingOutcomeObservation]
	checks atomic.Int32
}

func newDLFixture(t *testing.T, status db.IncidentStatus, po *db.PendingOutcome) *dlFixture {
	t.Helper()
	f := &dlFixture{}
	f.obs.Store(&agent.PendingOutcomeObservation{})
	ag := &scriptedPendingOutcomeAgent{
		checkOutcome: func(context.Context, *db.PendingOutcome) (*agent.PendingOutcomeObservation, error) {
			f.checks.Add(1)
			return f.obs.Load(), nil
		},
	}
	f.svc, f.db, f.notif = newPendingOutcomeTestService(t, ag)
	f.inc = newTrackedIncident(t, f.db, status, po)
	return f
}

func (f *dlFixture) snap(t *testing.T) lifecycle.Snapshot {
	t.Helper()
	s, err := f.db.GetIncidentSnapshot(t.Context(), f.inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func freshTrack() *db.PendingOutcome {
	return &db.PendingOutcome{
		MediaType: "tv", Title: "Tracked", Season: 1, Episode: 1, StartedAt: time.Now(), LastProgressAt: time.Now(),
	}
}

func TestI2_2_TrackingStartsWithPendingOutcome(t *testing.T) {
	t.Parallel()
	ag := &scriptedPendingOutcomeAgent{
		runResult: &agent.DiagnosticResult{
			RootCause: "missing", Confidence: "high", PrimaryAction: agent.ToolArrSearchMissing, PrimaryReason: "t",
		},
		searchParams: map[string]any{"media_type": "tv", "title": "Andor", "season": float64(2), "episode": float64(3)},
	}
	svc, database, _ := newPendingOutcomeTestService(t, ag)
	ag.database = database
	inc, err := svc.Handle(t.Context(), &incident.Report{
		Source: "discord", ReportedBy: "alice", What: "cant_play", Title: "Andor",
	})
	if err != nil {
		t.Fatal(err)
	}
	var s lifecycle.Snapshot
	eventually(t, "awaiting download", func() bool {
		s, err = database.GetIncidentSnapshot(t.Context(), inc.ID)
		return err == nil && s.Status == lifecycle.AwaitingDownload
	})
	if s.Download == nil || s.Download.Title != "Andor" || s.Download.Stage != lifecycle.StageSearching ||
		s.NextCheck.IsZero() {
		t.Fatalf("tracking %+v next %v", s.Download, s.NextCheck)
	}
	eventually(t, "the download check", func() bool { return svc.ArmedForTest(inc.ID) == s.Generation })
}

func TestI2_9_ServiceQueueDropImporting(t *testing.T) {
	t.Parallel()
	po := freshTrack()
	po.GrabNotified, po.LastStage, po.QueueStatus = true, "downloading", "downloading"
	f := newDLFixture(t, statusAwaiting, po)
	f.svc.CheckNowForTest(t.Context(), f.inc.ID)
	s := f.snap(t)
	if s.Status != lifecycle.AwaitingDownload || s.Download.Stage != lifecycle.StageImporting {
		t.Fatalf("after the queue item vanished: %s stage %s", s.Status, s.Download.Stage)
	}
}

func TestI2_16_OrphanPendingOutcomeNeverSwept(t *testing.T) {
	t.Parallel()
	f := newDLFixture(t, statusAwaiting, freshTrack())
	if _, err := f.svc.ApplyForTest(t.Context(), f.inc.ID, lifecycle.Event{Kind: lifecycle.OwnerResolve}); err != nil {
		t.Fatal(err)
	}
	if err := setTrack(t.Context(), f.db, f.inc.ID, freshTrack(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.svc.ReconcileForTest(t.Context())
	if f.svc.ArmedForTest(f.inc.ID) != 0 || f.checks.Load() != 0 {
		t.Fatalf("an orphan pending outcome on a resolved incident was armed or checked (%d checks)", f.checks.Load())
	}
}

func TestI2_18_DownloadCheckStaleGenerationRejected(t *testing.T) {
	t.Parallel()
	f := newDLFixture(t, statusAwaiting, freshTrack())
	old := f.snap(t).Generation
	if _, err := f.svc.ApplyForTest(t.Context(), f.inc.ID, lifecycle.Event{Kind: lifecycle.OwnerRerun}); err != nil {
		t.Fatal(err)
	}
	before := f.snap(t)
	f.svc.DownloadCheckForTest(t.Context(), f.inc.ID, old)
	if f.checks.Load() != 0 {
		t.Fatal("a stale download check still polled *arr")
	}
	if after := f.snap(t); after.Version != before.Version || after.Status != lifecycle.Reopened {
		t.Fatalf("a stale check changed the row: %+v", after)
	}
}

func TestI1_15_StaleDownloadCheckAfterRerunIgnored(t *testing.T) {
	t.Parallel()
	f := newDLFixture(t, statusAwaiting, freshTrack())
	f.svc.ReconcileForTest(t.Context())
	gen := f.snap(t).Generation
	if f.svc.ArmedForTest(f.inc.ID) != gen {
		t.Fatal("no download check armed")
	}
	if _, err := f.svc.ApplyForTest(t.Context(), f.inc.ID, lifecycle.Event{Kind: lifecycle.OwnerRerun}); err != nil {
		t.Fatal(err)
	}
	if got := f.svc.ArmedForTest(f.inc.ID); got != 0 {
		t.Fatalf("the old generation's check stayed armed (gen %d)", got)
	}
}

func TestI1_16_LeftoverPendingOutcomeDroppedOnRerun(t *testing.T) {
	t.Parallel()
	f := newDLFixture(t, db.StatusManualTestNeeded, nil)
	if f.snap(t).Download == nil {
		t.Fatal("no_release seed has no pending outcome")
	}
	if _, err := f.svc.ApplyForTest(t.Context(), f.inc.ID, lifecycle.Event{Kind: lifecycle.OwnerRerun}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.GetPendingOutcome(t.Context(), f.inc.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("pending outcome after rerun: %v", err)
	}
}

func TestI5_12_DownloadCheckerUnderGeneration(t *testing.T) {
	t.Parallel()
	f := newDLFixture(t, statusAwaiting, freshTrack())
	gen := f.snap(t).Generation
	f.svc.CheckNowForTest(t.Context(), f.inc.ID)
	s := f.snap(t)
	if s.Generation != gen || s.Status != lifecycle.AwaitingDownload {
		t.Fatalf("a reschedule changed the generation: %d -> %d (%s)", gen, s.Generation, s.Status)
	}
	eventually(t, "the recheck under the same generation", func() bool { return f.svc.ArmedForTest(f.inc.ID) == gen })
}

func TestDownload_StallEscalatesWithoutKeepSearching(t *testing.T) {
	t.Parallel()
	stale := time.Now().Add(-3 * time.Hour)
	po := freshTrack()
	po.StartedAt, po.LastProgressAt, po.LastProgressPct, po.GrabNotified = stale, stale, 40, true
	po.LastStage = "downloading"
	f := newDLFixture(t, statusAwaiting, po)
	f.obs.Store(&agent.PendingOutcomeObservation{QueueStage: "downloading", ProgressPct: 40})
	f.svc.CheckNowForTest(t.Context(), f.inc.ID)
	s := f.snap(t)
	if s.Status != lifecycle.ManualTestNeeded || s.Reason != lifecycle.ReasonDownloadProblem || s.Download != nil {
		t.Fatalf("stalled download: %s/%s pending %v", s.Status, s.Reason, s.Download != nil)
	}
	if lifecycle.Offers(s, time.Now(), lifecycle.ActKeepSearching) {
		t.Fatal("a stalled download offers Keep searching")
	}
	f.notif.mu.Lock()
	defer f.notif.mu.Unlock()
	if len(f.notif.msgs) == 0 || strings.Contains(f.notif.msgs[0], "Keep searching") {
		t.Fatalf("owner card %q", f.notif.msgs)
	}
}

func TestDownload_QueueStatusKeptSeparately(t *testing.T) {
	t.Parallel()
	f := newDLFixture(t, statusAwaiting, freshTrack())
	f.obs.Store(&agent.PendingOutcomeObservation{QueueStage: "importPending", ProgressPct: 100})
	f.svc.CheckNowForTest(t.Context(), f.inc.ID)
	d := f.snap(t).Download
	if d == nil || d.Stage != lifecycle.StageDownloading || d.QueueStatus != "importPending" {
		t.Fatalf("download %+v", d)
	}
}
