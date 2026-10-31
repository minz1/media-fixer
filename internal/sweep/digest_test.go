package sweep_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func stuckRow(key, title string, season, streak int) db.SweepTarget {
	return db.SweepTarget{
		Target: key, SeriesID: 1, SeriesTitle: title, Season: season, Streak: streak, Missing: 4,
		Attempts:     streak + 1,
		LastAttempt:  time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC),
		NextEligible: time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC),
	}
}

func TestStuck(t *testing.T) {
	t.Parallel()
	rows := map[string]db.SweepTarget{
		"s:1:1": stuckRow("s:1:1", "Alpha", 1, 2),
		"s:1:2": stuckRow("s:1:2", "Alpha", 2, 3),
		"s:2:1": stuckRow("s:2:1", "Beta", 1, 5),
	}
	got := sweep.Stuck(rows, 3)
	if len(got) != 2 || got[0].Target != "s:2:1" || got[1].Target != "s:1:2" {
		t.Fatalf("Stuck = %+v, want s:2:1 then s:1:2", got)
	}
	rows["s:0:3"] = stuckRow("s:0:3", "Alpha", 1, 3)
	rows["s:0:1"] = stuckRow("s:0:1", "Aaa", 9, 3)
	got = sweep.Stuck(rows, 3)
	want := []string{"s:2:1", "s:0:1", "s:0:3", "s:1:2"}
	for i, w := range want {
		if got[i].Target != w {
			t.Fatalf("order = %+v, want %v (streak desc, title, season)", got, want)
		}
	}
}

func TestDigestCard(t *testing.T) {
	t.Parallel()
	var rows []db.SweepTarget
	for i := 1; i <= 12; i++ {
		rows = append(rows, stuckRow(fmt.Sprintf("s:1:%d", i), "Show", i, 3))
	}
	c := sweep.DigestCardForTest(rows)
	lines := strings.Split(c.Body, "\n")
	if len(lines) != 11 || lines[10] != "…and 2 more" {
		t.Fatalf("body = %q, want 10 lines and …and 2 more", c.Body)
	}
	if want := "Show S01: 4 missing, 4 attempts, last 2026-10-20, next 2026-11-03"; lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
	if c.Subject != "" || c.Title != "Stuck seasons" || c.Tone != card.Warn || c.Path != "/babysit" {
		t.Errorf("card = %+v", c)
	}
	e := sweep.DigestCardForTest(nil)
	if e.Body != "No seasons are stuck now." || e.Tone != card.Info || e.Subject != "" {
		t.Errorf("empty card = %+v", e)
	}
}

func day0() time.Time { return time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) }

func at(d, hour, minute int) time.Time {
	return day0().AddDate(0, 0, d).Add(time.Duration(hour*60+minute) * time.Minute)
}

func digestSetup(t *testing.T, s sweep.Settings) (*harness, *sweep.Scheduler) {
	t.Helper()
	s.IntervalHours = 168
	return scheduledAt(t, day0().Add(-48*time.Hour), s)
}

func (h *harness) seedStuck(t *testing.T, key string, season int) {
	t.Helper()
	if err := h.db.UpsertSweepTarget(context.Background(), stuckRow(key, "Show", season, 3)); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) digests() []card.Card {
	cards, _ := h.sent()
	var out []card.Card
	for _, c := range cards {
		if c.Title == "Stuck seasons" {
			out = append(out, c)
		}
	}
	return out
}

func TestScheduler_DigestOncePerDay(t *testing.T) {
	t.Parallel()
	h, sch := digestSetup(t, settings())
	h.seedStuck(t, "s:1:1", 1)
	h.tick(sch, at(0, 8, 59))
	if n := len(h.digests()); n != 0 {
		t.Fatalf("digests before the hour = %d, want 0", n)
	}
	h.tick(sch, at(0, 9, 0))
	for m := 1; m < 15*60; m += 7 {
		h.tick(sch, at(0, 9, 0).Add(time.Duration(m)*time.Minute))
	}
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests on day 0 = %d, want 1", n)
	}
	h.tick(sch, at(1, 9, 0))
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests with an unchanged list = %d, want 1", n)
	}
	h.seedStuck(t, "s:1:2", 2)
	h.tick(sch, at(1, 9, 1))
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests later on a day already checked = %d, want 1", n)
	}
	h.tick(sch, at(2, 9, 0))
	if n := len(h.digests()); n != 2 {
		t.Fatalf("digests after a new stuck season = %d, want 2", n)
	}
}

func TestScheduler_DigestMissedHour(t *testing.T) {
	t.Parallel()
	h, sch := digestSetup(t, settings())
	h.seedStuck(t, "s:1:1", 1)
	h.tick(sch, at(0, 14, 0))
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests at 14:00 = %d, want 1", n)
	}
	h.seedStuck(t, "s:1:2", 2)
	h.tick(sch, at(1, 8, 59))
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests at 08:59 next day = %d, want 1", n)
	}
}

func TestScheduler_DigestRestart(t *testing.T) {
	t.Parallel()
	h, sch := digestSetup(t, settings())
	h.seedStuck(t, "s:1:1", 1)
	h.tick(sch, at(0, 9, 0))
	h.seedStuck(t, "s:1:2", 2)
	sch = h.restart(at(0, 9, 5))
	h.tick(sch, at(0, 9, 5))
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests after restart = %d, want 1", n)
	}
}

func TestScheduler_DigestRecovery(t *testing.T) {
	t.Parallel()
	h, sch := digestSetup(t, settings())
	h.tick(sch, at(0, 9, 0))
	if n := len(h.digests()); n != 0 {
		t.Fatalf("digests with nothing stuck and no history = %d, want 0", n)
	}
	h.seedStuck(t, "s:1:1", 1)
	h.tick(sch, at(1, 9, 0))
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests = %d, want 1", n)
	}
	if err := h.db.DeleteSweepTargets(context.Background(), []string{"s:1:1"}); err != nil {
		t.Fatal(err)
	}
	h.tick(sch, at(2, 9, 0))
	got := h.digests()
	if len(got) != 2 || got[1].Body != "No seasons are stuck now." {
		t.Fatalf("digests = %+v, want a recovery card second", got)
	}
	h.tick(sch, at(3, 9, 0))
	if n := len(h.digests()); n != 2 {
		t.Fatalf("digests after empty after empty = %d, want 2", n)
	}
}

func TestScheduler_DigestDisabledSweep(t *testing.T) {
	t.Parallel()
	s := settings()
	s.Enabled = false
	h, sch := digestSetup(t, s)
	h.seedStuck(t, "s:1:1", 1)
	h.tick(sch, at(0, 9, 0))
	if n := len(h.digests()); n != 0 {
		t.Fatalf("digests with the sweep disabled = %d, want 0", n)
	}
}

func TestScheduler_DigestErrorDoesNotBlockRun(t *testing.T) {
	t.Parallel()
	h, sch := scheduledAt(t, day0().Add(-48*time.Hour), settings())
	logs := &lockedBuffer{}
	h.runner.Log = slog.New(slog.NewTextHandler(logs, nil))
	h.seedStuck(t, "s:1:1", 1)
	notify := h.runner.Notify
	fail := true
	h.runner.Notify = func(ctx context.Context, c card.Card) error {
		if c.Title == "Stuck seasons" && fail {
			return errors.New("discord down")
		}
		return notify(ctx, c)
	}
	h.tick(sch, at(0, 9, 0))
	if n := len(h.runs(t)); n != 1 {
		t.Fatalf("runs = %d, want the due run despite the digest error", n)
	}
	if n := logs.count("WARN"); n != 1 {
		t.Fatalf("WARNs = %d, want 1", n)
	}

	h.setPaused(t, true)
	h.seedStuck(t, "s:1:2", 2)
	h.tick(sch, at(1, 9, 0))
	if n := logs.count("WARN"); n != 1 {
		t.Fatalf("WARNs = %d, want the same error warned once while paused", n)
	}
	if n := logs.count("INFO"); n != 0 {
		t.Fatalf("cleared lines = %d, want the digest error kept while paused", n)
	}
	if n := len(h.digests()); n != 0 {
		t.Fatalf("digests = %d, want none while Notify fails", n)
	}

	fail = false
	h.tick(sch, at(2, 9, 0))
	if n := len(h.digests()); n != 1 {
		t.Fatalf("digests after recovery = %d, want the changed list sent", n)
	}
}
