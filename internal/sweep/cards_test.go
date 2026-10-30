package sweep_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

func TestSummaryCard(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	end := start.Add(83 * time.Second)
	counts := db.SweepCounts{Grabs: 1, MaxGrabs: 10, Lookups: 2, MaxLookups: 40, Searched: 3, MaxSearches: 200}
	cases := []struct {
		name string
		sr   db.SweepRun
		tone card.Tone
		want string
	}{
		{"done", db.SweepRun{Status: "done", StartedAt: start, FinishedAt: &end, Counts: counts}, card.Success,
			"Pack sweep done:\ngrabs 1/10, lookups 2/40, searched 3/200, imported 0, failed 0, refused 0, " +
				"still downloading 0, took 1m23s. /media/maintenance"},
		{
			"dry refused",
			db.SweepRun{Dry: true, Status: "refused", Reason: "why", StartedAt: start, FinishedAt: &end},
			card.Warn,
			"Pack sweep dry run refused (why):\ngrabs 0/0, lookups 0/0, searched 0/0, imported 0, failed 0, " +
				"refused 0, still downloading 0, took 1m23s. /media/maintenance",
		},
		{"unfinished", db.SweepRun{Status: "failed", StartedAt: start}, card.Warn,
			"Pack sweep failed:\ngrabs 0/0, lookups 0/0, searched 0/0, imported 0, failed 0, refused 0, " +
				"still downloading 0. /media/maintenance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := sweep.SummaryCardForTest(tc.sr)
			if c.Plain() != tc.want || c.Tone != tc.tone || c.Path != "/maintenance" {
				t.Errorf("card = %+v\nplain %q\nwant  %q", c, c.Plain(), tc.want)
			}
		})
	}
}

func renderSweep(t *testing.T, r *sweep.Runner, id int64) (card.Update, bool) {
	t.Helper()
	up, ok, err := r.CardRenderer()(context.Background(), card.SweepSubject(id))
	if err != nil {
		t.Fatal(err)
	}
	return up, ok
}

func sectionText(up card.Update) string {
	var b strings.Builder
	for _, f := range up.Sections {
		b.WriteString(f.Name + ": " + f.Value + "\n")
	}
	return b.String()
}

func TestRenderer_LiveProgress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	id, err := h.db.CreateSweepRun(ctx, "manual", false, t0())
	if err != nil {
		t.Fatal(err)
	}
	sweep.SetProgressForTest(h.runner, &sweep.Progress{
		RunID: id, Phase: "season pass", Current: "Show S01",
		Counts: db.SweepCounts{Grabs: 1, MaxGrabs: 10, Lookups: 2, MaxLookups: 40, MaxSearches: 200},
	})
	up, ok := renderSweep(t, h.runner, id)
	if !ok || !up.Live || up.Tone != nil {
		t.Fatalf("update = %+v ok=%v, want live with no tone", up, ok)
	}
	if !strings.Contains(up.Status, "season pass") || !strings.Contains(up.Status, "Show S01") ||
		!strings.Contains(sectionText(up), "grabs 1/10, lookups 2/40, searched 0/200") {
		t.Errorf("update = %+v", up)
	}
	if _, ok = renderSweep(t, h.runner, id+1); ok {
		t.Error("an unknown run rendered ok")
	}
}

func TestRenderer_RunningWithoutProgressUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id, err := h.db.CreateSweepRun(context.Background(), "manual", false, t0())
	if err != nil {
		t.Fatal(err)
	}
	if up, ok := renderSweep(t, h.runner, id); ok {
		t.Errorf("running row with no progress rendered %+v", up)
	}
	if _, ok, rerr := h.runner.CardRenderer()(context.Background(), "sweep:x"); ok || rerr != nil {
		t.Errorf("bad subject: ok=%v err=%v", ok, rerr)
	}
}

func TestRenderer_Finished(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	for _, tc := range []struct {
		status, reason string
		tone           card.Tone
	}{
		{"done", "", card.Success},
		{"failed", "sonarr down", card.Warn},
	} {
		id, err := h.db.CreateSweepRun(ctx, "scheduled", false, t0())
		if err != nil {
			t.Fatal(err)
		}
		c := db.SweepCounts{Grabs: 1, MaxGrabs: 10, Lookups: 2, MaxLookups: 40, Imported: 1}
		if err = h.db.FinishSweepRun(ctx, id, tc.status, tc.reason, c, t0().Add(83*time.Second)); err != nil {
			t.Fatal(err)
		}
		up, ok := renderSweep(t, h.runner, id)
		if !ok || up.Live || up.Tone == nil || *up.Tone != tc.tone {
			t.Fatalf("%s: update = %+v ok=%v", tc.status, up, ok)
		}
		if !strings.Contains(up.Status, tc.status) || !strings.Contains(up.Status, tc.reason) ||
			!strings.Contains(up.Status, "took 1m23s") || !strings.Contains(sectionText(up), "imported 1") {
			t.Errorf("%s: update = %+v", tc.status, up)
		}
	}
}

func TestRenderer_Interrupted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	id, err := h.db.CreateSweepRun(ctx, "manual", false, t0())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.db.InterruptRunningSweeps(ctx); err != nil {
		t.Fatal(err)
	}
	up, ok := renderSweep(t, h.runner, id)
	if !ok || up.Live || up.Tone == nil || *up.Tone != card.Warn || !strings.Contains(up.Status, "interrupted") ||
		strings.Contains(up.Status, "took") {
		t.Errorf("update = %+v ok=%v", up, ok)
	}
}
