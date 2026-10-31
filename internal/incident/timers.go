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
	LockAlertGapMinutes     int `json:"lock_alert_gap_minutes"`
	NoReleaseGraceMinutes   int `json:"no_release_grace_minutes"`
	StallMinutes            int `json:"stall_minutes"`
	TrackCapHours           int `json:"track_cap_hours"`
	KeepSearchingCheckHours int `json:"keep_searching_check_hours"`
	KeepSearchingDays       int `json:"keep_searching_days"`
	ExplainTimeoutSeconds   int `json:"explain_timeout_seconds"`
	StuckReadTasks          int `json:"stuck_read_tasks"`
	StuckReadMinutes        int `json:"stuck_read_minutes"`
}

type TimerField struct {
	Group, Name, Label, Unit string
	Lo, Hi, Default, Value   int
	Help                     string
}

type timerDef struct {
	group, name, label, unit string
	lo, hi, def              int
	help                     string
}

type timerSpec struct {
	f TimerField
	p *int
}

func spec(p *int, d timerDef) timerSpec {
	return timerSpec{TimerField{
		Group: d.group, Name: d.name, Label: d.label, Unit: d.unit,
		Lo: d.lo, Hi: d.hi, Default: d.def, Value: *p, Help: d.help,
	}, p}
}

func (t *Timers) table() []timerSpec {
	const (
		runs     = "Agent runs"
		verify   = "Verification"
		notes    = "Reporter notes"
		download = "Download tracking"
		explain  = "Explain"
		stuck    = "Stuck reads"
		mins     = "minutes"
	)
	return []timerSpec{
		spec(&t.RetryFirstMinutes, timerDef{
			runs, "retry_first_minutes", "Wait before the first retry", mins, 1, 30, 2,
			"After a failed agent run, wait this long before retrying it.",
		}),
		spec(&t.RetrySecondMinutes, timerDef{
			runs, "retry_second_minutes", "Wait before the second retry", mins, 1, 60, 6,
			"If the retry fails too, wait this long before the second and last retry.",
		}),
		spec(&t.StaleRunMinutes, timerDef{
			runs, "stale_run_minutes", "Rerun a run with no heartbeat after", mins, 10, 240, 15,
			"A run with no heartbeat for this long is treated as dead and rerun.",
		}),
		spec(&t.CheckEveryMinutes, timerDef{
			runs, "check_every_minutes", "Stale-run check interval", mins, 1, 60, 5,
			"How often to look for dead runs and check tracked downloads.",
		}),
		spec(&t.FirstVerifyCapMinutes, timerDef{
			verify, "first_verify_cap_minutes", "First verification wait cap", mins, 5, 120, 30,
			"Longest wait before the first check that a fix worked.",
		}),
		spec(&t.VerifyGapMinutes, timerDef{
			verify, "verify_gap_minutes", "Gap between verification checks", mins, 1, 30, 2,
			"Longest wait between the later verification checks.",
		}),
		spec(&t.VerifyChecks, timerDef{
			verify, "verify_checks", "Verification checks", "checks", 1, 20, 5,
			"How many checks a fix gets before it counts as failed.",
		}),
		spec(&t.ApprovedVerifyMinutes, timerDef{
			verify, "approved_verify_minutes", "Verify after an approved fix", mins, 1, 120, 10,
			"After you approve an escalation, wait this long before verifying it.",
		}),
		spec(&t.RerunNoteGapMinutes, timerDef{
			notes, "rerun_note_gap_minutes", "Gap between rerun notes", mins, 5, 1440, 30,
			"At most one re-running note to the reporter per this gap.",
		}),
		spec(&t.ProgressNoteGapMinutes, timerDef{
			notes, "progress_note_gap_minutes", "Gap between progress notes", mins, 15, 1440, 60,
			"At most one progress note to the reporter per this gap.",
		}),
		spec(&t.SeerrAlertGapMinutes, timerDef{
			notes, "seerr_alert_gap_minutes", "Gap between Seerr alerts", mins, 15, 1440, 60,
			"After a Seerr failure alert, later Seerr failures in this gap are only logged.",
		}),
		spec(&t.LockAlertGapMinutes, timerDef{
			notes, "lock_alert_gap_minutes", "Gap between systemic lock alerts", mins, 60, 10080, 1440,
			"While the systemic lock holds, alert you again after this long.",
		}),
		spec(&t.NoReleaseGraceMinutes, timerDef{
			download, "no_release_grace_minutes", "Grace for a release that hasn't appeared", mins, 5, 240, 15,
			"Escalate when a search has found no release after this long.",
		}),
		spec(&t.StallMinutes, timerDef{
			download, "stall_minutes", "Escalate a download with no progress after", mins, 15, 720, 60,
			"Escalate a tracked download that makes no progress for this long.",
		}),
		spec(&t.TrackCapHours, timerDef{
			download, "track_cap_hours", "Stop tracking a download after", "hours", 1, 72, 12,
			"Stop tracking a download after this long, finished or not.",
		}),
		spec(&t.KeepSearchingCheckHours, timerDef{
			download, "keep_searching_check_hours", "Keep searching: check every", "hours", 1, 24, 1,
			"With keep searching on, look for a release this often.",
		}),
		spec(&t.KeepSearchingDays, timerDef{
			download, "keep_searching_days", "Keep searching: give up after", "days", 1, 30, 7,
			"With keep searching on, escalate again after this many days.",
		}),
		spec(&t.ExplainTimeoutSeconds, timerDef{
			explain, "explain_timeout_seconds", "Explain: give up after", "seconds", 30, 600, 120,
			"Stop waiting for an Explain answer after this long.",
		}),
		spec(&t.StuckReadTasks, timerDef{
			stuck, "stuck_read_tasks", "Stuck tasks on one torrent", "tasks", 2, 10, 3,
			"Open a stuck-read incident when this many tasks are stuck on one torrent.",
		}),
		spec(&t.StuckReadMinutes, timerDef{
			stuck, "stuck_read_minutes", "One task stuck for", mins, 2, 60, 5,
			"Or when one task has been stuck reading for this long.",
		}),
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
