package stuckqueue

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
	SettingsKey = "maintenance.stuck_queue"

	defaultIntervalMinutes = 60
	defaultMinAgeHours     = 6
	lowestIntervalMinutes  = 15
	highestIntervalMinutes = 1440
	lowestMinAgeHours      = 1
	highestMinAgeHours     = 168
)

var ErrCorruptSettings = errors.New("stored stuck-queue settings are unreadable")

type Settings struct {
	Enabled         bool `json:"enabled"`
	DryRun          bool `json:"dry_run"`
	IntervalMinutes int  `json:"interval_minutes"`
	MinAgeHours     int  `json:"min_age_hours"`
}

func DefaultSettings() Settings {
	return Settings{
		Enabled: true, DryRun: true,
		IntervalMinutes: defaultIntervalMinutes, MinAgeHours: defaultMinAgeHours,
	}
}

func (s Settings) Validate() map[string]string {
	errs := map[string]string{}
	for _, f := range []struct {
		name      string
		v, lo, hi int
	}{
		{"interval_minutes", s.IntervalMinutes, lowestIntervalMinutes, highestIntervalMinutes},
		{"min_age_hours", s.MinAgeHours, lowestMinAgeHours, highestMinAgeHours},
	} {
		if f.v < f.lo || f.v > f.hi {
			errs[f.name] = fmt.Sprintf("must be between %d and %d", f.lo, f.hi)
		}
	}
	return errs
}

func (s Settings) Interval() time.Duration { return time.Duration(s.IntervalMinutes) * time.Minute }

func (s Settings) MinAge() time.Duration { return time.Duration(s.MinAgeHours) * time.Hour }

func LoadSettings(ctx context.Context, d *db.DB) (Settings, error) {
	raw, err := d.GetSetting(ctx, SettingsKey)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return disabledDefaults(), err
	}
	var s Settings
	if err = json.Unmarshal([]byte(raw), &s); err != nil {
		return disabledDefaults(), fmt.Errorf("%w: %w", ErrCorruptSettings, err)
	}
	if errs := s.Validate(); len(errs) != 0 {
		return disabledDefaults(), fmt.Errorf("%w: %v", ErrCorruptSettings, errs)
	}
	return s, nil
}

func SaveSettings(ctx context.Context, d *db.DB, s Settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return d.SetSetting(ctx, SettingsKey, string(b))
}

func disabledDefaults() Settings {
	s := DefaultSettings()
	s.Enabled = false
	return s
}

type Bounds struct {
	IntervalLo, IntervalHi, MinAgeLo, MinAgeHi int
}

func SettingBounds() Bounds {
	return Bounds{lowestIntervalMinutes, highestIntervalMinutes, lowestMinAgeHours, highestMinAgeHours}
}
