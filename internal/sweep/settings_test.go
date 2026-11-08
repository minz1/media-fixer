package sweep_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
}

func storedObject(t *testing.T, h *harness) map[string]json.RawMessage {
	t.Helper()
	stored, err := h.db.GetSetting(context.Background(), sweep.SettingsKey)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal([]byte(stored), &obj); err != nil {
		t.Fatalf("stored row %q: %v", stored, err)
	}
	return obj
}

func decodedKey(t *testing.T, obj map[string]json.RawMessage, key string) any {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("key %q missing from stored row", key)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestS6_Copies_SaveKeepsUnknownKeys(t *testing.T) {
	t.Parallel()
	want := map[string]any{"weekday": float64(3), "hour_utc": float64(7), "future_key": map[string]any{"a": "<b>"}}
	for name, act := range map[string]func(context.Context, *harness) error{
		"SaveSettings": func(ctx context.Context, h *harness) error {
			return sweep.SaveSettings(ctx, h.db, settings(), t0())
		},
		"SetExcluded": func(ctx context.Context, h *harness) error {
			return sweep.SetExcluded(ctx, h.db, 5, true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			ctx := context.Background()
			raw := fmt.Sprintf(`{"enabled":true,"weekday":3,"hour_utc":7,"interval_hours":6,"enabled_at":%q,`+
				`"future_key":{"a":"<b>"}}`, t0().Format(time.RFC3339))
			if err := h.db.SetSetting(ctx, sweep.SettingsKey, raw); err != nil {
				t.Fatal(err)
			}
			if err := act(ctx, h); err != nil {
				t.Fatal(err)
			}
			obj := storedObject(t, h)
			for key, v := range want {
				if got := decodedKey(t, obj, key); !reflect.DeepEqual(got, v) {
					t.Errorf("%s = %v, want %v", key, got, v)
				}
			}
		})
	}
}

func TestS6_Copies_DisableNullsEnabledAt(t *testing.T) {
	t.Parallel()
	at := t0().Format(time.RFC3339)
	t.Run("disable save", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		ctx := context.Background()
		seedSettings(t, h, fmt.Sprintf(`{"enabled":true,"interval_hours":6,"enabled_at":%q}`, at))
		off := settings()
		off.Enabled = false
		if err := sweep.SaveSettings(ctx, h.db, off, t0().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got := decodedKey(t, storedObject(t, h), "enabled_at"); got != nil {
			t.Errorf("stored enabled_at = %v, want null", got)
		}
		loaded, err := sweep.LoadSettings(ctx, h.db)
		if err != nil || loaded.EnabledAt != nil {
			t.Errorf("loaded enabled_at = %v, %v; want nil", loaded.EnabledAt, err)
		}
	})
	t.Run("exclude on enabled row", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		ctx := context.Background()
		seedSettings(t, h, fmt.Sprintf(`{"enabled":true,"interval_hours":6,"enabled_at":%q}`, at))
		if err := sweep.SetExcluded(ctx, h.db, 5, true); err != nil {
			t.Fatal(err)
		}
		if got := decodedKey(t, storedObject(t, h), "enabled_at"); got != at {
			t.Errorf("stored enabled_at = %v, want %s", got, at)
		}
	})
	t.Run("exclude on legacy disabled row", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		ctx := context.Background()
		seedSettings(t, h, `{"enabled":false,"interval_hours":6}`)
		if err := sweep.SetExcluded(ctx, h.db, 5, true); err != nil {
			t.Fatal(err)
		}
		if _, ok := storedObject(t, h)["enabled_at"]; ok {
			t.Error("SetExcluded added an enabled_at key to a legacy disabled row")
		}
	})
}

func TestS6_6_CorruptRowSaveReplaces(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		raw  string
		keep bool
	}{{`[]`, false}, {`{not json`, false}, {`{"max_grabs":"x"}`, false}, {`{"max_grabs":"x","future_key":1}`, true}} {
		t.Run(c.raw, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			ctx := context.Background()
			if err := h.db.SetSetting(ctx, sweep.SettingsKey, c.raw); err != nil {
				t.Fatal(err)
			}
			if err := sweep.SaveSettings(ctx, h.db, settings(), t0()); err != nil {
				t.Fatalf("SaveSettings over %q: %v", c.raw, err)
			}
			got, err := sweep.LoadSettings(ctx, h.db)
			if err != nil || len(got.ExcludedSeries) != 0 || got.MaxGrabs != 5 {
				t.Errorf("loaded %+v, %v; want a clean row with no exclusions", got, err)
			}
			_, kept := storedObject(t, h)["future_key"]
			if kept != c.keep {
				t.Errorf("future_key kept = %v, want %v", kept, c.keep)
			}
		})
	}
}

func TestS6_1_LegacyLiveShapeLoads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	raw := `{"enabled":true,"weekday":3,"hour_utc":7,"min_missing":1,"max_grabs":10,"max_lookups":40,` +
		`"max_searches":200,"anime":true,"excluded_series":[4],"enabled_at":"2026-10-29T12:00:00Z"}`
	if err := h.db.SetSetting(ctx, sweep.SettingsKey, raw); err != nil {
		t.Fatal(err)
	}
	got, err := sweep.LoadSettings(ctx, h.db)
	if err != nil || got.IntervalHours != 168 || got.EnabledAt == nil || !got.Enabled {
		t.Errorf("legacy live shape = %+v, %v", got, err)
	}
}

func seedSettings(t *testing.T, h *harness, raw string) {
	t.Helper()
	if err := h.db.SetSetting(context.Background(), sweep.SettingsKey, raw); err != nil {
		t.Fatal(err)
	}
}
