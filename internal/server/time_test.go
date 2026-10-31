package server_test

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/server"
)

func TestRelTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{-2 * time.Second, "just now"},
		{-12 * time.Second, "12s ago"},
		{-4 * time.Minute, "4m ago"},
		{-3 * time.Hour, "3h ago"},
		{-5 * 24 * time.Hour, "5d ago"},
		{-40 * 24 * time.Hour, "Sep 21"},
		{-400 * 24 * time.Hour, "Sep 26, 2025"},
		{12 * time.Second, "in 12s"},
		{4 * time.Minute, "in 4m"},
		{3 * time.Hour, "in 3h"},
		{5 * 24 * time.Hour, "in 5d"},
		{40 * 24 * time.Hour, "Dec 10"},
	} {
		if got := server.RelTime(now, now.Add(c.d)); got != c.want {
			t.Errorf("RelTime(now%+v) = %q, want %q", c.d, got, c.want)
		}
	}
	if got := server.RelTime(now, time.Time{}); got != "never" {
		t.Errorf("RelTime(zero) = %q, want never", got)
	}
}

var (
	timeAttrRe   = regexp.MustCompile(`\s(title|datetime)="[^"]*"`)
	weekdayRe    = regexp.MustCompile(`\b(Mon|Tue|Wed|Thu|Fri|Sat|Sun) [A-Z][a-z]{2} \d`)
	isoDateRe    = regexp.MustCompile(`\d{4}-\d{2}-\d{2}( \d{2}:\d{2})?`)
	exactTitleRe = regexp.MustCompile(`<time datetime="[^"]+" title="[^"]+ UTC">`)
)

func TestTimes_OneFormat(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{".Format", "timeAgo"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("template still uses %s", bad)
		}
	}

	env := newMaintenanceTestServer(t)
	ctx := context.Background()
	id, err := env.db.CreateSweepRun(ctx, "manual", true, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err = env.db.FinishSweepRun(ctx, id, "done", "", db.SweepCounts{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = env.db.UpsertSweepTarget(ctx, db.SweepTarget{
		Target: "s:1:2", SeriesID: 1, SeriesTitle: "Severance", Season: 2, Streak: 3, Missing: 9, Attempts: 5,
		LastAttempt: time.Now().Add(-48 * time.Hour), NextEligible: time.Now().Add(72 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	mon := newMonitorTestServer(t)
	mon.stuckSeven(t)

	for name, body := range map[string]string{
		"maintenance": do(t, env.srv, http.MethodGet, "/media/maintenance", nil).Body.String(),
		"monitoring":  do(t, mon.srv, http.MethodGet, "/media/monitoring/7", nil).Body.String(),
	} {
		if !exactTitleRe.MatchString(body) {
			t.Errorf("%s: no <time> with an exact UTC title", name)
		}
		visible := timeAttrRe.ReplaceAllString(body, "")
		for _, re := range []*regexp.Regexp{weekdayRe, isoDateRe} {
			if m := re.FindString(visible); m != "" {
				t.Errorf("%s: absolute time %q shown as text", name, m)
			}
		}
	}
}
