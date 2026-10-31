package sweep

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	raw, err := d.GetSetting(ctx, SettingsKey)
	if errors.Is(err, sql.ErrNoRows) {
		return parseSettings("", false)
	}
	if err != nil {
		return DefaultSettings(), err
	}
	return parseSettings(raw, true)
}

func parseSettings(raw string, found bool) (Settings, error) {
	s := DefaultSettings()
	if !found {
		return s, nil
	}
	s.IntervalHours = legacyIntervalHours
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return DefaultSettings(), fmt.Errorf("%w: %w", ErrCorruptSettings, err)
	}
	return s, nil
}

func updateSettings(ctx context.Context, d *db.DB, edit func(Settings, error) (Settings, error)) error {
	return d.UpdateSetting(ctx, SettingsKey, func(raw string, found bool) (string, error) {
		s, err := edit(parseSettings(raw, found))
		if err != nil {
			return "", err
		}
		if s.ExcludedSeries == nil {
			s.ExcludedSeries = []int{}
		}
		b, err := json.Marshal(s)
		return string(b), err
	})
}

func SetExcluded(ctx context.Context, d *db.DB, seriesID int, excluded bool) error {
	return updateSettings(ctx, d, func(s Settings, err error) (Settings, error) {
		if err != nil {
			return s, err
		}
		s.ExcludedSeries = slices.DeleteFunc(slices.Clone(s.ExcludedSeries), func(id int) bool {
			return id == seriesID
		})
		if excluded {
			s.ExcludedSeries = append(s.ExcludedSeries, seriesID)
		}
		slices.Sort(s.ExcludedSeries)
		return s, nil
	})
}

func SaveSettings(ctx context.Context, d *db.DB, s Settings, now time.Time) error {
	return updateSettings(ctx, d, func(prev Settings, err error) (Settings, error) {
		if err != nil && !errors.Is(err, ErrCorruptSettings) {
			return s, err
		}
		s.Weekday, s.HourUTC, s.ExcludedSeries = prev.Weekday, prev.HourUTC, prev.ExcludedSeries
		switch {
		case !s.Enabled:
			s.EnabledAt = nil
		case prev.Enabled && prev.IntervalHours == s.IntervalHours:
			s.EnabledAt = prev.EnabledAt
		default:
			at := now.UTC()
			s.EnabledAt = &at
		}
		return s, nil
	})
}
