package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func storeLimits() lifecycle.Limits {
	return lifecycle.Limits{
		MaxActions: 3, SystemicThreshold: 5, VerifyChecks: 3, ExtensionsMax: 3, Minute: time.Minute,
		FirstVerifyCap: 10 * time.Minute, VerifyGap: 5 * time.Minute, StuckReadFloor: 3 * time.Minute,
		ApprovedVerify: 5 * time.Minute, CheckEvery: 15 * time.Minute, NoReleaseGrace: 30 * time.Minute,
		Stall: time.Hour, TrackCap: 12 * time.Hour, KeepSearchingCheck: time.Hour, KeepSearchingSpan: 72 * time.Hour,
		PlanTTL: 10 * time.Minute,
	}
}

func next(ev lifecycle.Event) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		cur.In.Limits = storeLimits()
		d, err := lifecycle.Next(cur, ev, time.Now())
		return db.Decision{Decision: d}, err
	}
}

func force(edit func(s *lifecycle.Snapshot)) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		edit(&n)
		return db.Decision{Next: n}, nil
	}
}

func report(title string) *db.NewIncident {
	return &db.NewIncident{
		Source: "discord", ReportedBy: "someone", ReporterDiscordID: "u-1", What: "cant_play", Title: title,
	}
}

func created(t *testing.T, d *db.DB, title string) lifecycle.Snapshot {
	t.Helper()
	a, err := d.CreateFromReport(context.Background(), report(title), next(lifecycle.Event{Kind: lifecycle.Reported}))
	if err != nil {
		t.Fatal(err)
	}
	return a.Snap
}

func snap(t *testing.T, d *db.DB, id string) lifecycle.Snapshot {
	t.Helper()
	s, err := d.GetIncidentSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func claimed(t *testing.T, d *db.DB, title string) lifecycle.Snapshot {
	t.Helper()
	s := created(t, d, title)
	a, err := d.ApplyIncident(
		context.Background(),
		s.ID,
		next(lifecycle.Event{Kind: lifecycle.RunClaimed, Gen: s.Generation}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return a.Snap
}

func TestApplyIncident_RefusesIllegalSnapshot(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	s := created(t, d, "Illegal")
	_, err := d.ApplyIncident(context.Background(), s.ID, force(func(n *lifecycle.Snapshot) {
		n.Status, n.Locked = lifecycle.Blocked, false
	}))
	if !errors.Is(err, lifecycle.ErrIllegal) {
		t.Fatalf("err = %v, want ErrIllegal", err)
	}
	if got := snap(t, d, s.ID); got.Status != lifecycle.Open {
		t.Fatalf("status %s after a refused Apply", got.Status)
	}
}

func TestApplyIncident_VersionDerivedFromOwnerView(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Versioned")
	if s.Version != 1 {
		t.Fatalf("claim version %d, want 1", s.Version)
	}
	a, err := d.ApplyIncident(ctx, s.ID, force(func(n *lifecycle.Snapshot) { n.Version = 99 }))
	if err != nil {
		t.Fatal(err)
	}
	if a.Snap.Version != 1 || snap(t, d, s.ID).Version != 1 {
		t.Fatalf("a decide-set version was stored: %d", a.Snap.Version)
	}
	a, err = d.ApplyIncident(ctx, s.ID, next(lifecycle.Event{Kind: lifecycle.ActionAttempted, Gen: s.Generation}))
	if err != nil {
		t.Fatal(err)
	}
	if a.Snap.Version != 2 || a.Snap.ActionCount != 1 {
		t.Fatalf("after an action: version %d count %d", a.Snap.Version, a.Snap.ActionCount)
	}
}

func TestApplyIncident_ExemptKindsSkipVersion(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Exempt")
	if _, err := d.ApplyIncident(ctx, s.ID, next(lifecycle.Event{Kind: lifecycle.OwnerApprove})); !errors.Is(
		err, lifecycle.ErrChanged) {
		t.Fatalf("approve without a version: %v", err)
	}
	a, err := d.ApplyIncident(ctx, s.ID, next(lifecycle.Event{Kind: lifecycle.OwnerResolve}))
	if err != nil {
		t.Fatalf("resolve without a version: %v", err)
	}
	if a.Snap.Status != lifecycle.Resolved {
		t.Fatalf("status %s", a.Snap.Status)
	}
}

func TestApplyIncident_StaleVersionRefused(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Stale version")
	ev := lifecycle.Event{Kind: lifecycle.OwnerKeepSearching, HasVersion: true, Version: s.Version - 1}
	if _, err := d.ApplyIncident(ctx, s.ID, next(ev)); !errors.Is(err, lifecycle.ErrChanged) {
		t.Fatalf("err = %v, want ErrChanged", err)
	}
	if got := snap(t, d, s.ID); got.Version != s.Version || got.Status != s.Status {
		t.Fatalf("a refused Apply changed the row: %+v", got)
	}
}

func TestApplyIncident_JournalOnlyWritesNoRow(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Journal only")
	before, err := d.GetIncident(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	finding := lifecycle.Finding{JSON: json.RawMessage(`{"root_cause":"stale"}`)}
	ev := lifecycle.Event{Kind: lifecycle.AttemptFinished, Gen: s.Generation - 1, Finding: &finding}
	decide := func(cur lifecycle.Snapshot) (db.Decision, error) {
		dec, nerr := next(ev)(cur)
		dec.Events = []*db.Event{{IncidentID: cur.ID, Kind: "run_finished", Payload: json.RawMessage(`{}`)}}
		return dec, nerr
	}
	a, err := d.ApplyIncident(ctx, s.ID, decide)
	if err != nil {
		t.Fatal(err)
	}
	after, err := d.GetIncident(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.Finding != nil || after.Version != before.Version {
		t.Fatalf("journal-only Apply wrote the row: %+v", after)
	}
	if len(a.Events) != 1 || a.Events[0].Seq == 0 ||
		eventKinds(t, d, s.ID)[len(eventKinds(t, d, s.ID))-1] != "run_finished" {
		t.Fatalf("run_finished not appended: %+v", a.Events)
	}
}

func TestApplyIncident_EventsInSameTx(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := created(t, d, "Same tx")
	n := len(eventKinds(t, d, s.ID))
	withEvent := func(edit func(*lifecycle.Snapshot)) db.Decide {
		return func(cur lifecycle.Snapshot) (db.Decision, error) {
			dec, err := force(edit)(cur)
			dec.Events = []*db.Event{{IncidentID: cur.ID, Kind: "marker", Payload: json.RawMessage(`{}`)}}
			return dec, err
		}
	}
	if _, err := d.ApplyIncident(
		ctx,
		s.ID,
		withEvent(func(n *lifecycle.Snapshot) { n.Parked = true; n.Status = lifecycle.Verifying }),
	); err == nil {
		t.Fatal("an illegal snapshot was accepted")
	}
	if got := len(eventKinds(t, d, s.ID)); got != n {
		t.Fatalf("a refused Apply appended %d events", got-n)
	}
	a, err := d.ApplyIncident(ctx, s.ID, withEvent(func(n *lifecycle.Snapshot) { n.Parked = true }))
	if err != nil {
		t.Fatal(err)
	}
	kinds := eventKinds(t, d, s.ID)[n:]
	if fmt.Sprint(kinds) != "[marker status_changed]" || len(a.Events) != 2 {
		t.Fatalf("events %v, applied %d", kinds, len(a.Events))
	}
	var p struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if err = json.Unmarshal(a.Events[1].Payload, &p); err != nil || p.Status != "open" || p.Version != 1 {
		t.Fatalf("status_changed payload %s %v", a.Events[1].Payload, err)
	}
}

func TestApplyIncident_PausedReadInsideTx(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := created(t, d, "Paused inside")
	created(t, d, "Another active")
	if err := d.SetSetting(ctx, "autonomous_paused", "true"); err != nil {
		t.Fatal(err)
	}
	var seen lifecycle.Inputs
	_, err := d.ApplyIncident(ctx, s.ID, func(cur lifecycle.Snapshot) (db.Decision, error) {
		seen = cur.In
		return db.Decision{}, lifecycle.ErrNoop
	})
	if !errors.Is(err, lifecycle.ErrNoop) {
		t.Fatal(err)
	}
	if !seen.Paused || seen.ActiveCount != 2 {
		t.Fatalf("inputs %+v, want paused with 2 active", seen)
	}
}

func TestI1_18_ConcurrentSameTitleReportsMerge(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	const n = 8
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		merged int
		ids    = map[string]bool{}
	)
	for i := range n {
		wg.Go(func() {
			r := report("Same Title (2024)")
			r.ReportedBy, r.ReporterDiscordID = fmt.Sprintf("user-%d", i), fmt.Sprintf("u-%d", i)
			a, err := d.CreateFromReport(ctx, r, next(lifecycle.Event{Kind: lifecycle.Reported}))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[a.Snap.ID] = true
			if a.Merged {
				merged++
			}
		})
	}
	wg.Wait()
	if len(ids) != 1 || merged != n-1 {
		t.Fatalf("%d incidents, %d merged", len(ids), merged)
	}
	for id := range ids {
		reporters, err := d.ListReporters(ctx, id)
		if err != nil || len(reporters) != n {
			t.Fatalf("reporters %v %v", reporters, err)
		}
	}
}

func TestI1_17_SystemicReportBlockedAndLockedInOneWrite(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	for i := range 5 {
		created(t, d, fmt.Sprintf("Busy %d", i))
	}
	a, err := d.CreateFromReport(
		context.Background(),
		report("Systemic"),
		next(lifecycle.Event{Kind: lifecycle.Reported}),
	)
	if err != nil {
		t.Fatal(err)
	}
	got := snap(t, d, a.Snap.ID)
	if got.Status != lifecycle.Blocked || !got.Locked || got.Generation != 0 {
		t.Fatalf("systemic report stored as %s locked=%v gen=%d", got.Status, got.Locked, got.Generation)
	}
	if len(a.Effects) != 1 || a.Effects[0].Kind != lifecycle.NotifyOwner {
		t.Fatalf("effects %+v", a.Effects)
	}
	if kinds := fmt.Sprint(eventKinds(t, d, a.Snap.ID)); kinds != "[incident_created reporter_added]" {
		t.Fatalf("events %s", kinds)
	}
}

func lockedOutsideBlocked(ctx context.Context, d *db.DB) error {
	list, err := d.ListIncidents(ctx, "", 100, 0)
	if err != nil {
		return err
	}
	for _, inc := range list {
		if inc.AutonomousLocked && inc.Status != db.StatusBlocked {
			return fmt.Errorf("%s seen %s and locked", inc.ID, inc.Status)
		}
	}
	return nil
}

func TestI4_2_SystemicLockNeverOpenLocked(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	for i := range 5 {
		created(t, d, fmt.Sprintf("Load %d", i))
	}
	stop := make(chan struct{})
	var bad error
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if bad = lockedOutsideBlocked(ctx, d); bad != nil {
				return
			}
		}
	})
	for i := range 10 {
		if _, err := d.CreateFromReport(ctx, report(fmt.Sprintf("Systemic %d", i)),
			next(lifecycle.Event{Kind: lifecycle.Reported})); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if bad != nil {
		t.Fatal(bad)
	}
}

func TestI1_20_RerunWriteFailureChangesNothing(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := claimed(t, d, "Rerun fails")
	n := len(eventKinds(t, d, s.ID))
	if err := d.ExecForTest(ctx, `CREATE TRIGGER refuse_update BEFORE UPDATE OF status ON incidents
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyIncident(ctx, s.ID, next(lifecycle.Event{Kind: lifecycle.OwnerRerun})); err == nil {
		t.Fatal("rerun succeeded through a refusing trigger")
	}
	got := snap(t, d, s.ID)
	if got.Status != lifecycle.Investigating || got.Generation != s.Generation || got.Version != s.Version {
		t.Fatalf("a failed rerun changed the row: %+v", got)
	}
	if len(eventKinds(t, d, s.ID)) != n {
		t.Fatal("a failed rerun appended events")
	}
}

func TestCreateFromReport_MergeAddsReporterAndSeerrIssue(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	s := created(t, d, "Merge me")
	r := report("Merge me")
	r.ReportedBy, r.ReporterDiscordID, r.Source, r.SeerrIssueID = "other", "", "seerr", "77"
	a, err := d.CreateFromReport(ctx, r, next(lifecycle.Event{Kind: lifecycle.Reported}))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Merged || a.Snap.ID != s.ID || len(a.Effects) != 1 || a.Effects[0].Kind != lifecycle.SeerrComment {
		t.Fatalf("merge %+v", a)
	}
	a, err = d.CreateFromReport(ctx, r, next(lifecycle.Event{Kind: lifecycle.Reported}))
	if err != nil || len(a.Effects) != 0 {
		t.Fatalf("a repeated seerr issue asked for another comment: %+v %v", a.Effects, err)
	}
}

func TestListDriven_ReturnsDrivenRowsWithActivity(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	open := created(t, d, "Driven open")
	resolved := created(t, d, "Driven resolved")
	if _, err := d.ApplyIncident(ctx, resolved.ID, next(lifecycle.Event{Kind: lifecycle.OwnerResolve})); err != nil {
		t.Fatal(err)
	}
	rows, err := d.ListDriven(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Snap.ID != open.ID || rows[0].LastActivity.IsZero() {
		t.Fatalf("driven rows %+v", rows)
	}
}
