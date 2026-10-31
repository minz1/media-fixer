package sweep_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/sweep"
)

func TestLoadSettings_LegacyRowIs168h(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	raw := `{"enabled":true,"weekday":0,"hour_utc":12,"max_grabs":5,"max_lookups":60,"max_searches":0}`
	if err := h.db.SetSetting(ctx, sweep.SettingsKey, raw); err != nil {
		t.Fatal(err)
	}
	got, err := sweep.LoadSettings(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	if got.IntervalHours != 168 || got.MaxGrabs != 5 || got.MaxSearches != 0 || got.BackoffBaseHours != 24 ||
		got.BurstFailures != 5 || got.StuckAfter != 3 || got.DigestHourUTC != 9 {
		t.Errorf("legacy row = %+v", got)
	}
}

func TestLoadSettings_NoRowDefaults(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	got, err := sweep.LoadSettings(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	if got.IntervalHours != 6 || got.MaxLookups != 40 || got.MaxSearches != 200 || got.MaxGrabs != 10 {
		t.Errorf("defaults = %+v", got)
	}
}

func TestValidate_Bounds(t *testing.T) {
	t.Parallel()
	fields := []struct {
		name   string
		lo, hi int
		set    func(*sweep.Settings, int)
	}{
		{"interval_hours", 1, 168, func(s *sweep.Settings, v int) { s.IntervalHours = v }},
		{"min_missing", 1, 100, func(s *sweep.Settings, v int) { s.MinMissing = v }},
		{"max_grabs", 0, 100, func(s *sweep.Settings, v int) { s.MaxGrabs = v }},
		{"max_lookups", 0, 500, func(s *sweep.Settings, v int) { s.MaxLookups = v }},
		{"max_searches", 0, 2000, func(s *sweep.Settings, v int) { s.MaxSearches = v }},
		{"backoff_base_hours", 1, 168, func(s *sweep.Settings, v int) { s.BackoffBaseHours = v }},
		{"backoff_max_days", 1, 90, func(s *sweep.Settings, v int) { s.BackoffMaxDays = v }},
		{"burst_failures", 2, 100, func(s *sweep.Settings, v int) { s.BurstFailures = v }},
		{"stuck_after", 1, 20, func(s *sweep.Settings, v int) { s.StuckAfter = v }},
		{"digest_hour_utc", 0, 23, func(s *sweep.Settings, v int) { s.DigestHourUTC = v }},
	}
	for _, f := range fields {
		for _, c := range []struct {
			v  int
			ok bool
		}{{f.lo - 1, false}, {f.lo, true}, {f.hi, true}, {f.hi + 1, false}} {
			s := sweep.DefaultSettings()
			f.set(&s, c.v)
			errs := s.Validate()
			if _, bad := errs[f.name]; bad == c.ok || (c.ok && len(errs) != 0) || (!c.ok && len(errs) != 1) {
				t.Errorf("%s = %d: errs %v, want ok=%v", f.name, c.v, errs, c.ok)
			}
		}
	}
	s := sweep.DefaultSettings()
	s.Weekday, s.HourUTC = 9, 99
	if errs := s.Validate(); len(errs) != 0 {
		t.Errorf("weekday and hour_utc are still validated: %v", errs)
	}
}

func TestSaveSettings_KeepsStoredWeekdayHour(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	raw := fmt.Sprintf(`{"enabled":true,"weekday":3,"hour_utc":7,"interval_hours":6,"enabled_at":%q}`,
		t0().Format(time.RFC3339))
	if err := h.db.SetSetting(ctx, sweep.SettingsKey, raw); err != nil {
		t.Fatal(err)
	}
	s := settings()
	s.Weekday, s.HourUTC = 0, 0
	if err := sweep.SaveSettings(ctx, h.db, s, t0()); err != nil {
		t.Fatal(err)
	}
	stored, err := h.db.GetSetting(ctx, sweep.SettingsKey)
	if err != nil {
		t.Fatal(err)
	}
	var got sweep.Settings
	if err = json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatal(err)
	}
	if got.Weekday != 3 || got.HourUTC != 7 {
		t.Errorf("stored weekday/hour = %d/%d, want 3/7", got.Weekday, got.HourUTC)
	}
}
