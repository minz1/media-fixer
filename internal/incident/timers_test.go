package incident_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

func timersDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "timers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestDefaultTimers(t *testing.T) {
	t.Parallel()
	d := incident.DefaultTimers()
	want := []int{2, 6, 15, 5, 30, 2, 5, 10, 30, 60, 60, 15, 60, 12, 1, 7, 120}
	got := []int{
		d.RetryFirstMinutes, d.RetrySecondMinutes, d.StaleRunMinutes, d.CheckEveryMinutes, d.FirstVerifyCapMinutes,
		d.VerifyGapMinutes, d.VerifyChecks, d.ApprovedVerifyMinutes, d.RerunNoteGapMinutes, d.ProgressNoteGapMinutes,
		d.SeerrAlertGapMinutes, d.NoReleaseGraceMinutes, d.StallMinutes, d.TrackCapHours, d.KeepSearchingCheckHours,
		d.KeepSearchingDays, d.ExplainTimeoutSeconds,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestValidate_Bounds(t *testing.T) {
	t.Parallel()
	companions := map[string]map[string]int{
		"retry_first_minutes":      {"stale_run_minutes": 41},
		"retry_second_minutes":     {"stale_run_minutes": 67},
		"stale_run_minutes":        {"retry_first_minutes": 1, "retry_second_minutes": 4},
		"approved_verify_minutes":  {"first_verify_cap_minutes": 120},
		"first_verify_cap_minutes": {"approved_verify_minutes": 5},
		"stall_minutes":            {"track_cap_hours": 13},
		"no_release_grace_minutes": {"track_cap_hours": 5},
		"track_cap_hours":          {"stall_minutes": 59, "no_release_grace_minutes": 15},
	}
	base := incident.DefaultTimers()
	for _, f := range base.Fields() {
		for _, c := range []struct {
			v  int
			ok bool
		}{{f.Lo - 1, false}, {f.Lo, true}, {f.Hi, true}, {f.Hi + 1, false}} {
			t.Run(fmt.Sprintf("%s=%d", f.Name, c.v), func(t *testing.T) {
				t.Parallel()
				x := incident.DefaultTimers()
				for k, v := range companions[f.Name] {
					x.Set(k, v)
				}
				x.Set(f.Name, c.v)
				got, has := x.Validate()[f.Name]
				if c.ok && has {
					t.Fatalf("unexpected error %q", got)
				}
				want := fmt.Sprintf("must be between %d and %d", f.Lo, f.Hi)
				if !c.ok && got != want {
					t.Fatalf("error = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestValidate_CrossField(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		set  map[string]int
		key  string
		bad  bool
	}{
		{"stale too low", map[string]int{"stale_run_minutes": 12}, "stale_run_minutes", true},
		{"stale ok", map[string]int{"stale_run_minutes": 13}, "stale_run_minutes", false},
		{"approved over cap", map[string]int{"approved_verify_minutes": 31, "first_verify_cap_minutes": 30}, "approved_verify_minutes", true},
		{"track under stall", map[string]int{"track_cap_hours": 1, "stall_minutes": 60}, "track_cap_hours", true},
		{"track over stall", map[string]int{"track_cap_hours": 1, "stall_minutes": 59}, "track_cap_hours", false},
		{"track under grace", map[string]int{"track_cap_hours": 1, "stall_minutes": 59, "no_release_grace_minutes": 60}, "track_cap_hours", true},
		{"track over grace", map[string]int{"track_cap_hours": 1, "stall_minutes": 59, "no_release_grace_minutes": 59}, "track_cap_hours", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			x := incident.DefaultTimers()
			for k, v := range c.set {
				x.Set(k, v)
			}
			if _, has := x.Validate()[c.key]; has != c.bad {
				t.Errorf("error on %s = %v, want %v (%v)", c.key, has, c.bad, x.Validate())
			}
		})
	}
}

func TestValidate_CrossFieldSkippedOnFieldError(t *testing.T) {
	t.Parallel()
	x := incident.DefaultTimers()
	x.Set("retry_first_minutes", 0)
	x.Set("stale_run_minutes", 10)
	errs := x.Validate()
	if _, ok := errs["retry_first_minutes"]; !ok {
		t.Error("missing retry_first_minutes error")
	}
	if _, ok := errs["stale_run_minutes"]; ok {
		t.Errorf("cross-field error leaked: %v", errs)
	}
	y := incident.DefaultTimers()
	y.Set("stale_run_minutes", 5)
	if got := y.Validate()["stale_run_minutes"]; got != "must be between 10 and 240" {
		t.Errorf("stale_run_minutes = %q", got)
	}
}

func TestFields_MatchBoundsAndDefaults(t *testing.T) {
	t.Parallel()
	d := incident.DefaultTimers()
	fields := d.Fields()
	if len(fields) != 19 {
		t.Fatalf("len = %d", len(fields))
	}
	seen := map[string]bool{}
	for i, f := range fields {
		if seen[f.Name] {
			t.Errorf("duplicate %s", f.Name)
		}
		seen[f.Name] = true
		if f.Value != f.Default {
			t.Errorf("%s value %d != default %d", f.Name, f.Value, f.Default)
		}
		x := incident.DefaultTimers()
		x.Set(f.Name, f.Default+1)
		for j, g := range x.Fields() {
			if (g.Value != g.Default) != (j == i) {
				t.Errorf("Set(%s) changed field %s", f.Name, g.Name)
			}
		}
	}
	x := incident.DefaultTimers()
	if x.Set("nope", 1); x != incident.DefaultTimers() {
		t.Error("Set of unknown name changed a field")
	}
}

func TestLoadTimers_Missing(t *testing.T) {
	t.Parallel()
	got, err := incident.LoadTimers(context.Background(), timersDB(t))
	if err != nil || got != incident.DefaultTimers() {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestLoadTimers_BadRows(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{"corrupt": "{", "out of bounds": `{"check_every_minutes":0}`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := timersDB(t)
			if err := d.SetSetting(context.Background(), incident.TimersKey, raw); err != nil {
				t.Fatal(err)
			}
			got, err := incident.LoadTimers(context.Background(), d)
			if !errors.Is(err, incident.ErrCorruptTimers) || got != incident.DefaultTimers() {
				t.Errorf("got %+v, %v", got, err)
			}
		})
	}
}

func TestLoadTimers_PartialRow(t *testing.T) {
	t.Parallel()
	d := timersDB(t)
	if err := d.SetSetting(context.Background(), incident.TimersKey, `{"stall_minutes":90}`); err != nil {
		t.Fatal(err)
	}
	got, err := incident.LoadTimers(context.Background(), d)
	want := incident.DefaultTimers()
	want.StallMinutes = 90
	if err != nil || got != want || got.ExplainTimeoutSeconds != 120 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestSaveTimers_RoundTrip(t *testing.T) {
	t.Parallel()
	d := timersDB(t)
	want := incident.DefaultTimers()
	want.CheckEveryMinutes = 9
	want.KeepSearchingDays = 3
	if err := incident.SaveTimers(context.Background(), d, want); err != nil {
		t.Fatal(err)
	}
	got, err := incident.LoadTimers(context.Background(), d)
	if err != nil || got != want {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestTimersStuckReadFields(t *testing.T) {
	t.Parallel()
	d := incident.DefaultTimers()
	if d.StuckReadTasks != 3 || d.StuckReadMinutes != 5 {
		t.Errorf("defaults = %d, %d", d.StuckReadTasks, d.StuckReadMinutes)
	}
	for _, c := range []struct {
		name string
		v    int
		ok   bool
	}{
		{"stuck_read_tasks", 1, false}, {"stuck_read_tasks", 2, true},
		{"stuck_read_tasks", 10, true}, {"stuck_read_tasks", 11, false},
		{"stuck_read_minutes", 1, false}, {"stuck_read_minutes", 2, true},
		{"stuck_read_minutes", 60, true}, {"stuck_read_minutes", 61, false},
	} {
		x := incident.DefaultTimers()
		x.Set(c.name, c.v)
		if _, bad := x.Validate()[c.name]; bad == c.ok {
			t.Errorf("%s=%d: invalid=%v", c.name, c.v, bad)
		}
	}

	db := timersDB(t)
	want := incident.DefaultTimers()
	want.StuckReadTasks, want.StuckReadMinutes = 7, 12
	if err := incident.SaveTimers(context.Background(), db, want); err != nil {
		t.Fatal(err)
	}
	got, err := incident.LoadTimers(context.Background(), db)
	if err != nil || got != want {
		t.Errorf("round trip: got %+v, %v", got, err)
	}

	old := timersDB(t)
	if setErr := old.SetSetting(context.Background(), incident.TimersKey, `{"stall_minutes":90}`); setErr != nil {
		t.Fatal(setErr)
	}
	got, err = incident.LoadTimers(context.Background(), old)
	wantOld := incident.DefaultTimers()
	wantOld.StallMinutes = 90
	if err != nil || got != wantOld || got.StuckReadTasks != 3 || got.StuckReadMinutes != 5 {
		t.Errorf("old row: got %+v, %v", got, err)
	}

	units := map[string]string{}
	for _, f := range d.Fields() {
		if f.Group == "Stuck reads" {
			units[f.Name] = f.Unit
		}
	}
	if len(units) != 2 || units["stuck_read_tasks"] != "tasks" || units["stuck_read_minutes"] != "minutes" {
		t.Errorf("fields = %v", units)
	}
}
