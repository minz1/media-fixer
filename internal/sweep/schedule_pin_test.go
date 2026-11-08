package sweep_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func pinBase() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }

const pinSettingsRaw = `{"enabled":true,"weekday":1,"hour_utc":12,"min_missing":1,"max_grabs":10,"max_lookups":40,` +
	`"max_searches":200,"anime":true,"excluded_series":[3,7],"enabled_at":"2026-09-13T08:30:00Z"}`

func pinSeed(t *testing.T) (*harness, *sweep.Scheduler) {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.db.SetSetting(ctx, sweep.SettingsKey, pinSettingsRaw); err != nil {
		t.Fatal(err)
	}
	for k := range 5 {
		finishedRun(t, h, pinBase().Add(time.Duration(k)*168*time.Hour), sweepstate.Done, db.SweepCounts{})
	}
	slot := pinBase().Add(5 * 168 * time.Hour)
	finishedRun(t, h, slot.Add(time.Minute), sweepstate.Failed, db.SweepCounts{})
	finishedRun(t, h, slot.Add(2*time.Hour+5*time.Minute), sweepstate.Interrupted, db.SweepCounts{})
	finishedRun(t, h, slot.Add(5*time.Hour), sweepstate.Paused, db.SweepCounts{})
	return h, sweep.NewScheduler(ctx, h.runner, h.db)
}

func pinHash(t *testing.T, d *db.DB) string {
	t.Helper()
	raw, err := d.GetSetting(context.Background(), sweep.SettingsKey)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}

func TestQA1_NoScheduleDrift(t *testing.T) {
	t.Parallel()
	h, sch := pinSeed(t)
	before := pinHash(t, h.db)
	slot := pinBase().Add(5 * 168 * time.Hour)

	hourly := []struct{ fromHour, nextMin int }{{0, 186}, {168, 10080}, {336, 20160}}
	for k := range 337 {
		want := hourly[0].nextMin
		for _, p := range hourly {
			if k >= p.fromHour {
				want = p.nextMin
			}
		}
		now := slot.Add(time.Duration(k) * time.Hour)
		got := sch.NextRun(now)
		if got == nil || !got.Equal(slot.Add(time.Duration(want)*time.Minute)) {
			t.Fatalf("NextRun(slot+%dh) = %v, want slot+%dmin", k, got, want)
		}
	}

	boundaries := []struct {
		atMin   int
		delta   time.Duration
		nextMin int
	}{
		{0, -time.Minute, 0},
		{0, -1, 0},
		{0, 0, 186},
		{0, 1, 186},
		{0, time.Minute, 186},
		{186, -time.Minute, 186},
		{186, -1, 186},
		{186, 0, 186},
		{186, 1, 186},
		{186, time.Minute, 186},
		{10080, -time.Minute, 186},
		{10080, -1, 186},
		{10080, 0, 10080},
		{10080, 1, 10080},
		{10080, time.Minute, 10080},
		{
			10266,
			-time.Minute,
			10080,
		},
		{10266, -1, 10080},
		{10266, 0, 10080},
		{10266, 1, 10080},
		{10266, time.Minute, 10080},
		{
			20160,
			-time.Minute,
			10080,
		},
		{20160, -1, 10080},
		{20160, 0, 20160},
		{20160, 1, 20160},
		{20160, time.Minute, 20160},
	}
	for _, b := range boundaries {
		now := slot.Add(time.Duration(b.atMin)*time.Minute + b.delta)
		got := sch.NextRun(now)
		if got == nil || !got.Equal(slot.Add(time.Duration(b.nextMin)*time.Minute)) {
			t.Errorf("NextRun(slot+%dmin%+d) = %v, want slot+%dmin", b.atMin, b.delta, got, b.nextMin)
		}
	}

	probes := []struct {
		name  string
		at    time.Duration
		extra bool
		want  int
	}{
		{"just before the slot", -time.Minute, false, 0},
		{"at the slot", 0, false, 0},
		{"inside the retry window", time.Hour, false, 0},
		{"retry due", 3*time.Hour + 6*time.Minute, false, 1},
		{"past maxRetries", 8 * time.Hour, true, 0},
		{"after the following slot", 168*time.Hour + time.Minute, false, 1},
	}
	for _, p := range probes {
		ph, psch := pinSeed(t)
		if p.extra {
			finishedRun(t, ph, slot.Add(7*time.Hour), sweepstate.Failed, db.SweepCounts{})
		}
		had := len(ph.runs(t))
		ph.tick(psch, slot.Add(p.at))
		if got := len(ph.runs(t)) - had; got != p.want {
			t.Errorf("%s: new runs = %d, want %d", p.name, got, p.want)
		}
	}

	if after := pinHash(t, h.db); after != before {
		t.Errorf("settings row changed: %s -> %s", before, after)
	}
}

func TestQA1_NoScheduleDriftOnSnapshot(t *testing.T) {
	t.Parallel()
	src := os.Getenv("MF_SNAPSHOT")
	if src == "" {
		t.Skip("MF_SNAPSHOT not set")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err = os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	last, err := d.LastSweepRunStart(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sch := sweep.NewScheduler(ctx, &sweep.Runner{DB: d}, d)
	for k := range 337 {
		now := last.Add(time.Duration(k) * time.Hour)
		next := sch.NextRun(now)
		if next == nil {
			fmt.Fprintf(os.Stdout, "k=%d next=none due=false\n", k)
			continue
		}
		fmt.Fprintf(os.Stdout, "k=%d next=%s due=%t\n", k, next.UTC().Format(time.RFC3339Nano), !now.Before(*next))
	}
}
