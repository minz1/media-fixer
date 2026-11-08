package sweep_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

func disabled() sweep.Settings {
	s := settings()
	s.Enabled = false
	return s
}

func TestS6_5_DisableThroughSaveCancelsLiveRun(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	saved := make(chan error, 1)
	h.fake.beforePost = func() { saved <- sch.Save(context.Background(), disabled(), h.clock.Now()) }
	if err := sch.RunNow(t.Context(), "manual", false); err != nil {
		t.Fatal(err)
	}
	sweep.WaitRunsForTest(sch)
	if saveErr := <-saved; saveErr != nil {
		t.Fatalf("Save: %v", saveErr)
	}
	runs := h.runs(t)
	if len(runs) != 1 || runs[0].Status != sweepstate.Interrupted || runs[0].Reason != sweep.ErrDisabled.Error() {
		t.Errorf("runs = %+v, want one run interrupted with %q", runs, sweep.ErrDisabled.Error())
	}
	got, err := sweep.LoadSettings(context.Background(), h.db)
	if err != nil || got.Enabled || got.EnabledAt != nil {
		t.Errorf("stored settings = %+v, %v; want disabled with no enabled_at", got, err)
	}
}

type goneAfterDisable struct {
	context.Context
	db   *db.DB
	mu   sync.Mutex
	gone bool
	ch   chan struct{}
}

func (c *goneAfterDisable) check() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.gone {
		if set, err := sweep.LoadSettings(context.Background(), c.db); err == nil && !set.Enabled {
			c.gone = true
			close(c.ch)
		}
	}
	return c.gone
}

func (c *goneAfterDisable) Done() <-chan struct{} {
	c.check()
	return c.ch
}

func (c *goneAfterDisable) Err() error {
	if c.check() {
		return context.Canceled
	}
	return nil
}

func TestS6_5_DisableCancelsLiveRunWhenTheRequestEndsAfterCommit(t *testing.T) {
	t.Parallel()
	for i := range 8 {
		h, sch := scheduledAt(t, t0(), settings())
		h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
		h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
		h.fake.plan("g1", grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
		ctx := &goneAfterDisable{Context: context.Background(), db: h.db, ch: make(chan struct{})}
		saved := make(chan error, 1)
		h.fake.beforePost = func() { saved <- sch.Save(ctx, disabled(), h.clock.Now()) }
		if err := sch.RunNow(t.Context(), "manual", false); err != nil {
			t.Fatal(err)
		}
		sweep.WaitRunsForTest(sch)
		if saveErr := <-saved; saveErr != nil {
			t.Errorf("round %d: Save: %v", i, saveErr)
		}
		if runs := h.runs(t); len(runs) != 1 || runs[0].Status != sweepstate.Interrupted {
			t.Fatalf("round %d: runs = %+v, want the live run interrupted after the committed disable", i, runs)
		}
	}
}

func TestS6_5_DisableSparesDryRun(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, t0(), settings())
	h.fake.addSeries(1, "Show", missingEps(1, 1, 11, 12)...)
	h.fake.setReleases(seasonKey(1, 1), seasonRel("g1", "Show S01", "Comet", 10))
	saved := make(chan error, 1)
	h.fake.beforeLookup = func() { saved <- sch.Save(context.Background(), disabled(), h.clock.Now()) }
	if err := sch.RunNow(t.Context(), "manual", true); err != nil {
		t.Fatal(err)
	}
	sweep.WaitRunsForTest(sch)
	if saveErr := <-saved; saveErr != nil {
		t.Fatalf("Save: %v", saveErr)
	}
	if runs := h.runs(t); len(runs) != 1 || runs[0].Status != sweepstate.Done {
		t.Errorf("runs = %+v, want the dry run done", runs)
	}
}

func TestS6_5_SaveDuringShutdownStillCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sch, _, cancel := startedScheduler(t, h.runner, h.db)
	cancel(nil)
	if err := sch.Save(context.Background(), settings(), t0()); err != nil {
		t.Fatalf("enable during shutdown: %v", err)
	}
	if err := sch.Save(context.Background(), disabled(), t0().Add(time.Hour)); err != nil {
		t.Fatalf("disable during shutdown: %v", err)
	}
	got, err := sweep.LoadSettings(context.Background(), h.db)
	if err != nil || got.Enabled {
		t.Errorf("stored settings = %+v, %v; want the disable committed", got, err)
	}
}

func TestS6_2_to_6_4_EnabledAtTable(t *testing.T) {
	t.Parallel()
	stamp := t0().Add(-time.Hour)
	now := t0()
	on := func(interval int, at *time.Time) sweep.Settings {
		s := settings()
		s.IntervalHours, s.EnabledAt = interval, at
		return s
	}
	off := disabled()
	for _, tc := range []struct {
		name       string
		prev, next sweep.Settings
		want       *time.Time
	}{
		{"disabled save", off, off, nil},
		{"enable", off, on(168, nil), &now},
		{"re-save while enabled", on(168, &stamp), on(168, nil), &stamp},
		{"budgets-only change", on(168, &stamp), func() sweep.Settings {
			s := on(168, nil)
			s.MaxGrabs = 2
			return s
		}(), &stamp},
		{"interval change", on(168, &stamp), on(12, nil), &now},
		{"disable", on(168, &stamp), off, nil},
		{"legacy enabled row kept without a stamp", on(168, nil), on(168, nil), nil},
	} {
		got := sweep.EnabledAtForTest(tc.prev, tc.next, now)
		if (got == nil) != (tc.want == nil) || got != nil && !got.Equal(*tc.want) {
			t.Errorf("%s: enabledAt = %v, want %v", tc.name, got, tc.want)
		}
	}
}
