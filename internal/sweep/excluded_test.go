package sweep_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/minz1/mediafixer/internal/sweep"
)

func putExcluded(t *testing.T, h *harness, ids ...int) {
	t.Helper()
	for _, id := range ids {
		if err := sweep.SetExcluded(context.Background(), h.db, id, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSaveSettingsKeepsExcluded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	putExcluded(t, h, 3, 5)
	s := settings()
	s.ExcludedSeries = []int{9}
	if err := sweep.SaveSettings(ctx, h.db, s, t0()); err != nil {
		t.Fatal(err)
	}
	got, err := sweep.LoadSettings(ctx, h.db)
	if err != nil || !slices.Equal(got.ExcludedSeries, []int{3, 5}) {
		t.Errorf("excluded = %v, %v, want [3 5]", got.ExcludedSeries, err)
	}
}

func TestSetExcluded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	putExcluded(t, h, 3, 5)
	base, _ := sweep.LoadSettings(ctx, h.db)
	for _, step := range []struct {
		id   int
		on   bool
		want []int
	}{{7, true, []int{3, 5, 7}}, {7, true, []int{3, 5, 7}}, {3, false, []int{5, 7}}} {
		if err := sweep.SetExcluded(ctx, h.db, step.id, step.on); err != nil {
			t.Fatal(err)
		}
		got, err := sweep.LoadSettings(ctx, h.db)
		if err != nil || !slices.Equal(got.ExcludedSeries, step.want) {
			t.Fatalf("excluded = %v, %v, want %v", got.ExcludedSeries, err, step.want)
		}
		got.ExcludedSeries, base.ExcludedSeries = nil, nil
		if got.MaxGrabs != base.MaxGrabs || got.Enabled != base.Enabled || got.StuckAfter != base.StuckAfter {
			t.Errorf("other fields changed: %+v vs %+v", got, base)
		}
	}
}

func TestSetExcludedCorrupt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	const raw = `{not json`
	if err := h.db.SetSetting(ctx, sweep.SettingsKey, raw); err != nil {
		t.Fatal(err)
	}
	if err := sweep.SetExcluded(ctx, h.db, 4, true); !errors.Is(err, sweep.ErrCorruptSettings) {
		t.Fatalf("err = %v, want ErrCorruptSettings", err)
	}
	if got, _ := h.db.GetSetting(ctx, sweep.SettingsKey); got != raw {
		t.Errorf("row = %q, want unchanged", got)
	}
}

func TestSetExcludedConcurrentWithSave(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 1; i <= 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := sweep.SetExcluded(ctx, h.db, i, true); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			s := settings()
			s.MaxGrabs = 77
			if err := sweep.SaveSettings(ctx, h.db, s, t0()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := sweep.LoadSettings(ctx, h.db)
	if err != nil || got.MaxGrabs != 77 || len(got.ExcludedSeries) != 10 {
		t.Errorf("got MaxGrabs=%d excluded=%v err=%v", got.MaxGrabs, got.ExcludedSeries, err)
	}
}
