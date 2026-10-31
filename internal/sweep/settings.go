package sweep

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/db"
)

const (
	SettingsKey = "maintenance.pack_sweep"

	defaultHourUTC       = 12
	defaultIntervalHours = 6
	legacyIntervalHours  = 168
	defaultMaxGrabs      = 10
	defaultMaxLookups    = 40
	defaultMaxSearches   = 200
	defaultBackoffBase   = 24
	defaultBackoffMax    = 30
	defaultBurstFailures = 5
	defaultStuckAfter    = 3
	defaultDigestHour    = 9
)

type Settings struct {
	Enabled          bool       `json:"enabled"`
	Weekday          int        `json:"weekday"`
	HourUTC          int        `json:"hour_utc"`
	IntervalHours    int        `json:"interval_hours"`
	MinMissing       int        `json:"min_missing"`
	MaxGrabs         int        `json:"max_grabs"`
	MaxLookups       int        `json:"max_lookups"`
	MaxSearches      int        `json:"max_searches"`
	BackoffBaseHours int        `json:"backoff_base_hours"`
	BackoffMaxDays   int        `json:"backoff_max_days"`
	BurstFailures    int        `json:"burst_failures"`
	StuckAfter       int        `json:"stuck_after"`
	DigestHourUTC    int        `json:"digest_hour_utc"`
	Anime            bool       `json:"anime"`
	ExcludedSeries   []int      `json:"excluded_series"`
	EnabledAt        *time.Time `json:"enabled_at,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{
		HourUTC: defaultHourUTC, IntervalHours: defaultIntervalHours, MinMissing: 1, MaxGrabs: defaultMaxGrabs,
		MaxLookups: defaultMaxLookups, MaxSearches: defaultMaxSearches, BackoffBaseHours: defaultBackoffBase,
		BackoffMaxDays: defaultBackoffMax, BurstFailures: defaultBurstFailures, StuckAfter: defaultStuckAfter,
		DigestHourUTC: defaultDigestHour, Anime: true, ExcludedSeries: []int{},
	}
}

func (s Settings) Interval() time.Duration {
	return time.Duration(max(1, s.IntervalHours)) * time.Hour
}

func (s Settings) Validate() map[string]string {
	errs := map[string]string{}
	for _, f := range []struct {
		name      string
		v, lo, hi int
	}{
		{"interval_hours", s.IntervalHours, 1, 168},
		{"min_missing", s.MinMissing, 1, 100},
		{"max_grabs", s.MaxGrabs, 0, 100},
		{"max_lookups", s.MaxLookups, 0, 500},
		{"max_searches", s.MaxSearches, 0, 2000},
		{"backoff_base_hours", s.BackoffBaseHours, 1, 168},
		{"backoff_max_days", s.BackoffMaxDays, 1, 90},
		{"burst_failures", s.BurstFailures, 2, 100},
		{"stuck_after", s.StuckAfter, 1, 20},
		{"digest_hour_utc", s.DigestHourUTC, 0, 23},
	} {
		if f.v < f.lo || f.v > f.hi {
			errs[f.name] = fmt.Sprintf("must be between %d and %d", f.lo, f.hi)
		}
	}
	return errs
}

var ErrCorruptSettings = errors.New("stored sweep settings are unreadable")

func LoadSettings(ctx context.Context, d *db.DB) (Settings, error) {
	s := DefaultSettings()
	raw, err := d.GetSetting(ctx, SettingsKey)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.IntervalHours = legacyIntervalHours
	if err = json.Unmarshal([]byte(raw), &s); err != nil {
		return DefaultSettings(), fmt.Errorf("%w: %w", ErrCorruptSettings, err)
	}
	return s, nil
}

func SaveSettings(ctx context.Context, d *db.DB, s Settings, now time.Time) error {
	prev, err := LoadSettings(ctx, d)
	if err != nil && !errors.Is(err, ErrCorruptSettings) {
		return err
	}
	s.Weekday, s.HourUTC = prev.Weekday, prev.HourUTC
	switch {
	case !s.Enabled:
		s.EnabledAt = nil
	case prev.Enabled && prev.IntervalHours == s.IntervalHours:
		s.EnabledAt = prev.EnabledAt
	default:
		now = now.UTC()
		s.EnabledAt = &now
	}
	if s.ExcludedSeries == nil {
		s.ExcludedSeries = []int{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return d.SetSetting(ctx, SettingsKey, string(b))
}
