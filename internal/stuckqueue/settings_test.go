package stuckqueue_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "stuck.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestDefaultSettings(t *testing.T) {
	t.Parallel()
	got := stuckqueue.DefaultSettings()
	want := stuckqueue.Settings{Enabled: true, DryRun: true, IntervalMinutes: 60, MinAgeHours: 6}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	if errs := got.Validate(); len(errs) != 0 {
		t.Fatalf("defaults invalid: %v", errs)
	}
}

func TestValidate_Bounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		interval, minAge int
		wantKey          string
	}{
		{14, 6, "interval_minutes"},
		{1441, 6, "interval_minutes"},
		{15, 6, ""},
		{1440, 6, ""},
		{60, 0, "min_age_hours"},
		{60, 169, "min_age_hours"},
		{60, 1, ""},
		{60, 168, ""},
	} {
		s := stuckqueue.DefaultSettings()
		s.IntervalMinutes, s.MinAgeHours = tc.interval, tc.minAge
		errs := s.Validate()
		if tc.wantKey == "" && len(errs) != 0 {
			t.Errorf("%+v: unexpected errors %v", tc, errs)
		}
		if tc.wantKey != "" && errs[tc.wantKey] == "" {
			t.Errorf("%+v: want error on %s, got %v", tc, tc.wantKey, errs)
		}
	}
}

func TestLoadSettings_MissingRowIsDefault(t *testing.T) {
	t.Parallel()
	got, err := stuckqueue.LoadSettings(context.Background(), openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if got != stuckqueue.DefaultSettings() {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadSettings_CorruptRowIsDisabled(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ctx := context.Background()
	if err := d.SetSetting(ctx, stuckqueue.SettingsKey, `{not json`); err != nil {
		t.Fatal(err)
	}
	got, err := stuckqueue.LoadSettings(ctx, d)
	if !errors.Is(err, stuckqueue.ErrCorruptSettings) {
		t.Fatalf("err = %v, want ErrCorruptSettings", err)
	}
	if got.Enabled {
		t.Fatal("a corrupt row must load as disabled")
	}
}

func TestLoadSettings_OutOfBoundsIsCorrupt(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ctx := context.Background()
	raw := `{"enabled":true,"dry_run":false,"interval_minutes":0,"min_age_hours":6}`
	if err := d.SetSetting(ctx, stuckqueue.SettingsKey, raw); err != nil {
		t.Fatal(err)
	}
	got, err := stuckqueue.LoadSettings(ctx, d)
	if !errors.Is(err, stuckqueue.ErrCorruptSettings) {
		t.Fatalf("err = %v, want ErrCorruptSettings", err)
	}
	if got.Enabled {
		t.Fatal("an out-of-bounds row must load as disabled")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	d := openDB(t)
	ctx := context.Background()
	want := stuckqueue.Settings{Enabled: true, DryRun: false, IntervalMinutes: 30, MinAgeHours: 12}
	if err := stuckqueue.SaveSettings(ctx, d, want); err != nil {
		t.Fatal(err)
	}
	got, err := stuckqueue.LoadSettings(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.Interval().Minutes() != 30 || got.MinAge().Hours() != 12 {
		t.Fatalf("durations = %v, %v", got.Interval(), got.MinAge())
	}
}
