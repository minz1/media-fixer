package db_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func TestS1_11_FinishIsCompareAndSwap(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	id, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	first := db.SweepCounts{Grabs: 1}
	if err = d.FinishSweepRun(ctx, id, sweepstate.Done, "", first, sweepT0().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	err = d.FinishSweepRun(ctx, id, sweepstate.Failed, "late", db.SweepCounts{Grabs: 9}, sweepT0().Add(time.Hour))
	if !errors.Is(err, db.ErrSweepRunSettled) {
		t.Fatalf("second finish err = %v, want ErrSweepRunSettled", err)
	}
	r, err := d.GetSweepRun(ctx, id)
	if err != nil || r.Status != sweepstate.Done || r.Reason != "" || r.Counts != first ||
		r.FinishedAt == nil || !r.FinishedAt.Equal(sweepT0().Add(time.Minute)) {
		t.Errorf("row after the refused second finish = %+v, %v", r, err)
	}
}

func TestS1_13_FinishRefusesIllegalTarget(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	for _, tc := range []struct {
		status sweepstate.RunStatus
		reason string
	}{
		{sweepstate.Running, ""},
		{sweepstate.Crashed, ""},
		{sweepstate.Done, "a reason"},
		{sweepstate.Interrupted, ""},
		{sweepstate.Failed, ""},
		{sweepstate.Refused, ""},
	} {
		id, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0())
		if err != nil {
			t.Fatal(err)
		}
		err = d.FinishSweepRun(ctx, id, tc.status, tc.reason, db.SweepCounts{}, sweepT0().Add(time.Minute))
		if err == nil || errors.Is(err, db.ErrSweepRunSettled) || !errors.Is(err, db.ErrSweepFinishInvalid) {
			t.Errorf("finish %s/%q err = %v, want ErrSweepFinishInvalid", tc.status, tc.reason, err)
		}
		r, getErr := d.GetSweepRun(ctx, id)
		if getErr != nil || r.Status != sweepstate.Running || r.FinishedAt != nil || r.Reason != "" {
			t.Errorf("row after refused %s finish = %+v, %v", tc.status, r, getErr)
		}
	}
}

func TestS1_12_StartupReconcileMarksCrashed(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	settled, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0())
	if err != nil {
		t.Fatal(err)
	}
	doneAt := sweepT0().Add(time.Minute)
	if err = d.FinishSweepRun(ctx, settled, sweepstate.Done, "", db.SweepCounts{}, doneAt); err != nil {
		t.Fatal(err)
	}
	var running []int64
	for i := 1; i <= 2; i++ {
		id, createErr := d.CreateSweepRun(ctx, "schedule", false, sweepT0().Add(time.Duration(i)*time.Hour))
		if createErr != nil {
			t.Fatal(createErr)
		}
		running = append(running, id)
	}
	ids, err := d.RunningSweepIDs(ctx)
	slices.Sort(ids)
	if err != nil || !slices.Equal(ids, running) {
		t.Fatalf("RunningSweepIDs = %v, %v; want %v", ids, err, running)
	}
	got, err := d.CrashRunningSweeps(ctx)
	slices.Sort(got)
	if err != nil || !slices.Equal(got, running) {
		t.Fatalf("CrashRunningSweeps = %v, %v; want %v", got, err, running)
	}
	for _, id := range running {
		r, getErr := d.GetSweepRun(ctx, id)
		if getErr != nil || r.Status != sweepstate.Crashed || r.FinishedAt != nil || r.Reason != "" {
			t.Errorf("run %d after crash = %+v, %v", id, r, getErr)
		}
	}
	if r, getErr := d.GetSweepRun(ctx, settled); getErr != nil || r.Status != sweepstate.Done {
		t.Errorf("settled run after crash = %+v, %v", r, getErr)
	}
	again, err := d.CrashRunningSweeps(ctx)
	if err != nil || len(again) != 0 {
		t.Errorf("second CrashRunningSweeps = %v, %v; want none", again, err)
	}
	if ids, err = d.RunningSweepIDs(ctx); err != nil || len(ids) != 0 {
		t.Errorf("RunningSweepIDs after crash = %v, %v; want none", ids, err)
	}
}

func crashedFlag(t *testing.T, stored sweepstate.RunStatus, hasCounts bool) (bool, error) {
	t.Helper()
	path := tempDBPath(t)
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	counts := "NULL"
	if hasCounts {
		counts = "'{}'"
	}
	rawExec(t, path, fmt.Sprintf(
		`INSERT INTO sweep_runs (trigger, dry, started_at, status, counts)
		 VALUES ('schedule', 0, '2026-10-22T12:00:00Z', '%s', %s)`, stored, counts))
	d, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, crashed, err := d.LiveSearchesSince(context.Background(), sweepT0().Add(-time.Hour))
	return crashed, err
}

func TestS1_Copies_CrashedSQLMatchesGo(t *testing.T) {
	t.Parallel()
	for _, stored := range sweepstate.RunStatuses() {
		if !sweepstate.Meta(stored).Stored {
			continue
		}
		for _, hasCounts := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/counts=%v", stored, hasCounts), func(t *testing.T) {
				t.Parallel()
				crashed, err := crashedFlag(t, stored, hasCounts)
				want := sweepstate.Derive(stored, hasCounts) == sweepstate.Crashed
				if err != nil || crashed != want {
					t.Errorf("crashed = %v, %v; Derive says %v", crashed, err, want)
				}
			})
		}
	}
}

func TestS1_Copies_StatusListsFromMeta(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	ctx := context.Background()
	for i, status := range []sweepstate.RunStatus{
		sweepstate.Refused, sweepstate.Done, sweepstate.Paused, sweepstate.Failed, sweepstate.Interrupted,
	} {
		at := sweepT0().Add(time.Duration(i) * time.Hour)
		id, err := d.CreateSweepRun(ctx, "schedule", false, at)
		if err != nil {
			t.Fatal(err)
		}
		finish := at.Add(time.Minute)
		if err = d.FinishSweepRun(ctx, id, status, reasonFor(status), db.SweepCounts{}, finish); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.CreateSweepRun(ctx, "schedule", false, sweepT0().Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check := func(phase string, last time.Time, n int, latest time.Time) {
		t.Helper()
		got, err := d.LastSweepRunStart(ctx)
		if err != nil || got == nil || !got.Equal(last) {
			t.Errorf("%s: LastSweepRunStart = %v, %v; want %v", phase, got, err, last)
		}
		gotN, gotLatest, err := d.RetryableSweepRunsSince(ctx, sweepT0())
		if err != nil || gotN != n || !gotLatest.Equal(latest) {
			t.Errorf("%s: RetryableSweepRunsSince = %d, %v, %v; want %d, %v", phase, gotN, gotLatest, err, n, latest)
		}
	}
	check("running", sweepT0().Add(5*time.Hour), 2, sweepT0().Add(4*time.Hour+time.Minute))
	if _, err := d.CrashRunningSweeps(ctx); err != nil {
		t.Fatal(err)
	}
	check("crashed", sweepT0().Add(time.Hour), 3, sweepT0().Add(5*time.Hour))
}
