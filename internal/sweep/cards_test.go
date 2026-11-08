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
			"Pack sweep done:\ngrabs 1/10, lookups 2/40, searched 3/200 (limits: left today), imported 0, failed 0, " +
				"refused 0, still downloading 0, took 1m23s. /media/maintenance"},
		{
			"dry refused",
			db.SweepRun{Dry: true, Status: "refused", Reason: "why", StartedAt: start, FinishedAt: &end},
			card.Warn,
			"Pack sweep dry run refused (why):\ngrabs 0/0, lookups 0/0, searched 0/0, imported 0, failed 0, " +
				"refused 0, still downloading 0, took 1m23s. /media/maintenance",
		},
		{"unfinished", db.SweepRun{Status: "failed", StartedAt: start}, card.Warn,
			"Pack sweep failed:\ngrabs 0/0, lookups 0/0, searched 0/0 (limits: left today), imported 0, failed 0, " +
				"refused 0, still downloading 0. /media/maintenance"},
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

func TestCountsText_LeftToday(t *testing.T) {
	t.Parallel()
	c := db.SweepCounts{Grabs: 1, MaxGrabs: 5, Lookups: 4, MaxLookups: 40, MaxSearches: 200}
	live := "grabs 1/5, lookups 4/40, searched 0/200 (limits: left today), imported 0, failed 0, refused 0, still downloading 0"
	if got := sweep.CountsTextForTest(c, false); got != live {
		t.Errorf("live = %q, want %q", got, live)
	}
	if got := sweep.CountsTextForTest(c, true); strings.Contains(got, "left today") ||
		!strings.Contains(got, "searched 0/200, imported 0") {
		t.Errorf("dry = %q, want no suffix", got)
	}
}

func TestStartCard_LeftToday(t *testing.T) {
	t.Parallel()
	s := sweep.DefaultSettings()
	for dry, want := range map[bool]string{false: "5 grabs, 40 lookups, 200 searches left today", true: "10 grabs, 40 lookups, 200 searches"} {
		s.MaxGrabs = 10
		if !dry {
			s.MaxGrabs = 5
		}
		c := sweep.StartCardForTest(1, "manual", dry, s)
		if got := c.Fields[1].Value; got != want {
			t.Errorf("dry=%v: budgets = %q, want %q", dry, got, want)
		}
	}
}

func TestStartCard_SeriesTrigger(t *testing.T) {
	t.Parallel()
	c := sweep.StartCardForTest(1, db.BabysitTrigger+" NOVA", false, sweep.DefaultSettings())
	if got := c.Fields[0].Value; got != "Monitoring: NOVA" {
		t.Errorf("trigger = %q, want Monitoring: NOVA", got)
	}
}

func TestTriggerLabel(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"babysit: NOVA": "Monitoring: NOVA", "manual": "manual", "": ""} {
		if got := sweep.TriggerLabel(in); got != want {
			t.Errorf("TriggerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func renderSweep(t *testing.T, r *sweep.Runner, id int64) (card.Update, bool) {
	t.Helper()
	res, err := r.CardRenderer()(context.Background(), card.SweepSubject(id))
	if err != nil {
		t.Fatal(err)
	}
	return res.Update, res.Outcome == card.Render
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
	if res, rerr := h.runner.CardRenderer()(context.Background(), "sweep:x"); res.Outcome != card.Gone || rerr != nil {
		t.Errorf("bad subject: outcome=%v err=%v", res.Outcome, rerr)
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

func TestD1_10_PrunedSweepRunIsGone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	var ids []int64
	for range 2 {
		id, err := h.db.CreateSweepRun(ctx, "manual", false, t0())
		if err != nil {
			t.Fatal(err)
		}
		if err = h.db.FinishSweepRun(ctx, id, "done", "", db.SweepCounts{}, t0().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := h.db.PruneSweepRuns(ctx, 1, t0().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r, err := h.runner.CardRenderer()(ctx, card.SweepSubject(ids[0]))
	if err != nil || r.Outcome != card.Gone {
		t.Errorf("pruned run: outcome = %v err = %v, want Gone", r.Outcome, err)
	}
}

func TestSweepRenderer_Outcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, h *harness) (context.Context, string)
		want    card.Outcome
		wantErr bool
	}{
		{"subject not sweep:<int>", func(*testing.T, *harness) (context.Context, string) {
			return ctx, "sweep:x"
		}, card.Gone, false},
		{"live progress", func(t *testing.T, h *harness) (context.Context, string) {
			id := createRun(t, h)
			sweep.SetProgressForTest(h.runner, &sweep.Progress{RunID: id, Phase: "season pass"})
			return ctx, card.SweepSubject(id)
		}, card.Render, false},
		{"run not found", func(t *testing.T, h *harness) (context.Context, string) {
			return ctx, card.SweepSubject(createRun(t, h) + 1)
		}, card.Gone, false},
		{"running without live progress", func(t *testing.T, h *harness) (context.Context, string) {
			return ctx, card.SweepSubject(createRun(t, h))
		}, card.NotYet, false},
		{"db error", func(t *testing.T, h *harness) (context.Context, string) {
			id := createRun(t, h)
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			return cctx, card.SweepSubject(id)
		}, card.NotYet, true},
		{"finished", func(t *testing.T, h *harness) (context.Context, string) {
			id := createRun(t, h)
			if err := h.db.FinishSweepRun(ctx, id, "done", "", db.SweepCounts{}, t0().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			return ctx, card.SweepSubject(id)
		}, card.Render, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			rctx, subject := tc.setup(t, h)
			r, err := h.runner.CardRenderer()(rctx, subject)
			if r.Outcome != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("outcome = %v err = %v, want %v (error %v)", r.Outcome, err, tc.want, tc.wantErr)
			}
		})
	}
}

func createRun(t *testing.T, h *harness) int64 {
	t.Helper()
	id, err := h.db.CreateSweepRun(context.Background(), "manual", false, t0())
	if err != nil {
		t.Fatal(err)
	}
	return id
}
