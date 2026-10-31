package incident

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/minz1/mediafixer/internal/db"
)

const (
	TimersKey = "maintenance.incident_timers"

	minutesPerHour = 60
	minutesPerDay  = 1440
	staleMargin    = 5
)

var ErrCorruptTimers = errors.New("stored incident timers are unreadable")

type Timers struct {
	RetryFirstMinutes       int `json:"retry_first_minutes"`
	RetrySecondMinutes      int `json:"retry_second_minutes"`
	StaleRunMinutes         int `json:"stale_run_minutes"`
	CheckEveryMinutes       int `json:"check_every_minutes"`
	FirstVerifyCapMinutes   int `json:"first_verify_cap_minutes"`
	VerifyGapMinutes        int `json:"verify_gap_minutes"`
	VerifyChecks            int `json:"verify_checks"`
	ApprovedVerifyMinutes   int `json:"approved_verify_minutes"`
	RerunNoteGapMinutes     int `json:"rerun_note_gap_minutes"`
	ProgressNoteGapMinutes  int `json:"progress_note_gap_minutes"`
	SeerrAlertGapMinutes    int `json:"seerr_alert_gap_minutes"`
	NoReleaseGraceMinutes   int `json:"no_release_grace_minutes"`
	StallMinutes            int `json:"stall_minutes"`
	TrackCapHours           int `json:"track_cap_hours"`
	KeepSearchingCheckHours int `json:"keep_searching_check_hours"`
	KeepSearchingDays       int `json:"keep_searching_days"`
}

type TimerField struct {
	Group, Name, Label, Unit string
	Lo, Hi, Default, Value   int
}

type timerSpec struct {
	f TimerField
	p *int
}

func (t *Timers) table() []timerSpec {
	const (
		runs     = "Agent runs"
		verify   = "Verification"
		notes    = "Reporter notes"
		download = "Download tracking"
		mins     = "minutes"
	)
	s := func(f TimerField, p *int) timerSpec {
		f.Value = *p
		return timerSpec{f, p}
	}
	return []timerSpec{
		s(
			TimerField{runs, "retry_first_minutes", "Wait before the first retry", mins, 1, 30, 2, 0},
			&t.RetryFirstMinutes,
		),
		s(
			TimerField{runs, "retry_second_minutes", "Wait before the second retry", mins, 1, 60, 6, 0},
			&t.RetrySecondMinutes,
		),
		s(
			TimerField{runs, "stale_run_minutes", "Rerun a run with no heartbeat after", mins, 10, 240, 15, 0},
			&t.StaleRunMinutes,
		),
		s(TimerField{runs, "check_every_minutes", "Stale-run check interval", mins, 1, 60, 5, 0}, &t.CheckEveryMinutes),
		s(
			TimerField{verify, "first_verify_cap_minutes", "First verification wait cap", mins, 5, 120, 30, 0},
			&t.FirstVerifyCapMinutes,
		),
		s(
			TimerField{verify, "verify_gap_minutes", "Gap between verification checks", mins, 1, 30, 2, 0},
			&t.VerifyGapMinutes,
		),
		s(TimerField{verify, "verify_checks", "Verification checks", "checks", 1, 20, 5, 0}, &t.VerifyChecks),
		s(
			TimerField{verify, "approved_verify_minutes", "Verify after an approved fix", mins, 1, 120, 10, 0},
			&t.ApprovedVerifyMinutes,
		),
		s(
			TimerField{notes, "rerun_note_gap_minutes", "Gap between rerun notes", mins, 5, 1440, 30, 0},
			&t.RerunNoteGapMinutes,
		),
		s(
			TimerField{notes, "progress_note_gap_minutes", "Gap between progress notes", mins, 15, 1440, 60, 0},
			&t.ProgressNoteGapMinutes,
		),
		s(
			TimerField{notes, "seerr_alert_gap_minutes", "Gap between Seerr alerts", mins, 15, 1440, 60, 0},
			&t.SeerrAlertGapMinutes,
		),
		s(
			TimerField{
				download,
				"no_release_grace_minutes",
				"Grace for a release that hasn't appeared",
				mins,
				5,
				240,
				15,
				0,
			},
			&t.NoReleaseGraceMinutes,
		),
		s(
			TimerField{download, "stall_minutes", "Escalate a download with no progress after", mins, 15, 720, 60, 0},
			&t.StallMinutes,
		),
		s(
			TimerField{download, "track_cap_hours", "Stop tracking a download after", "hours", 1, 72, 12, 0},
			&t.TrackCapHours,
		),
		s(
			TimerField{download, "keep_searching_check_hours", "Keep searching: check every", "hours", 1, 24, 1, 0},
			&t.KeepSearchingCheckHours,
		),
		s(
			TimerField{download, "keep_searching_days", "Keep searching: give up after", "days", 1, 30, 7, 0},
			&t.KeepSearchingDays,
		),
	}
}

func DefaultTimers() Timers {
	var t Timers
	for _, f := range t.table() {
		*f.p = f.f.Default
	}
	return t
}

func (t *Timers) Fields() []TimerField {
	var out []TimerField
	for _, f := range t.table() {
		out = append(out, f.f)
	}
	return out
}

func (t *Timers) Set(name string, v int) {
	for _, f := range t.table() {
		if f.f.Name == name {
			*f.p = v
		}
	}
}

func (t *Timers) Validate() map[string]string {
	errs := map[string]string{}
	for _, f := range t.table() {
		if *f.p < f.f.Lo || *f.p > f.f.Hi {
			errs[f.f.Name] = fmt.Sprintf("must be between %d and %d", f.f.Lo, f.f.Hi)
		}
	}
	rule := func(key string, inputs []string, msg func() string, ok bool) {
		if _, set := errs[key]; set {
			return
		}
		for _, in := range inputs {
			if _, bad := errs[in]; bad {
				return
			}
		}
		if !ok {
			errs[key] = msg()
		}
	}
	need := t.RetryFirstMinutes + t.RetrySecondMinutes + staleMargin
	rule("stale_run_minutes", []string{"retry_first_minutes", "retry_second_minutes", "stale_run_minutes"},
		func() string { return fmt.Sprintf("must be at least %d (both retry waits plus %d)", need, staleMargin) },
		t.StaleRunMinutes >= need)
	rule("approved_verify_minutes", []string{"first_verify_cap_minutes", "approved_verify_minutes"},
		func() string {
			return fmt.Sprintf("must not exceed the first verification cap (%d)", t.FirstVerifyCapMinutes)
		},
		t.ApprovedVerifyMinutes <= t.FirstVerifyCapMinutes)
	capMin := t.TrackCapHours * minutesPerHour
	rule("track_cap_hours", []string{"stall_minutes", "no_release_grace_minutes", "track_cap_hours"},
		func() string { return "must be longer than the stall time and the no-release grace" },
		capMin > t.StallMinutes && capMin > t.NoReleaseGraceMinutes)
	return errs
}

func LoadTimers(ctx context.Context, d *db.DB) (Timers, error) {
	raw, err := d.GetSetting(ctx, TimersKey)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultTimers(), nil
	}
	if err != nil {
		return DefaultTimers(), err
	}
	t := DefaultTimers()
	if err = json.Unmarshal([]byte(raw), &t); err != nil {
		return DefaultTimers(), fmt.Errorf("%w: %w", ErrCorruptTimers, err)
	}
	if errs := t.Validate(); len(errs) > 0 {
		return DefaultTimers(), fmt.Errorf("%w: %v", ErrCorruptTimers, errs)
	}
	return t, nil
}

func SaveTimers(ctx context.Context, d *db.DB, t Timers) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return d.SetSetting(ctx, TimersKey, string(b))
}
