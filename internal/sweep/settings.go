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

	defaultHourUTC    = 12
	defaultMaxGrabs   = 5
	defaultMaxLookups = 60
)

type Settings struct {
	Enabled        bool       `json:"enabled"`
	Weekday        int        `json:"weekday"`
	HourUTC        int        `json:"hour_utc"`
	MinMissing     int        `json:"min_missing"`
	MaxGrabs       int        `json:"max_grabs"`
	MaxLookups     int        `json:"max_lookups"`
	MaxSearches    int        `json:"max_searches"`
	Anime          bool       `json:"anime"`
	ExcludedSeries []int      `json:"excluded_series"`
	EnabledAt      *time.Time `json:"enabled_at,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{
		HourUTC: defaultHourUTC, MinMissing: 1, MaxGrabs: defaultMaxGrabs, MaxLookups: defaultMaxLookups,
		Anime: true, ExcludedSeries: []int{},
	}
}

func (s Settings) Validate() map[string]string {
	errs := map[string]string{}
	for _, f := range []struct {
		name      string
		v, lo, hi int
	}{
		{"weekday", s.Weekday, 0, 6},
		{"hour_utc", s.HourUTC, 0, 23},
		{"min_missing", s.MinMissing, 1, 100},
		{"max_grabs", s.MaxGrabs, 1, 50},
		{"max_lookups", s.MaxLookups, 1, 500},
		{"max_searches", s.MaxSearches, 0, 500},
	} {
		if f.v < f.lo || f.v > f.hi {
			errs[f.name] = fmt.Sprintf("must be between %d and %d", f.lo, f.hi)
		}
	}
	return errs
}

func LoadSettings(ctx context.Context, d *db.DB) (Settings, error) {
	s := DefaultSettings()
	raw, err := d.GetSetting(ctx, SettingsKey)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
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
	switch {
	case !s.Enabled:
		s.EnabledAt = nil
	case prev.Enabled && prev.Weekday == s.Weekday && prev.HourUTC == s.HourUTC:
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
