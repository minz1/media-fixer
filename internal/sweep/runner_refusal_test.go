package sweep_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

const blockRefusedDeleteSQL = `CREATE TRIGGER block_refused_delete BEFORE DELETE ON sweep_refused
	BEGIN SELECT RAISE(FAIL, 'blocked'); END`

func refusalHarness(t *testing.T) (*harness, []string) {
	t.Helper()
	h := newHarness(t)
	h.fake.addSeries(1, "Survivor", missingEps(1, 22, 1, 2, 3)...)
	rels := loadReleases(t, "rel-sv-22.json.gz")
	for i := range rels {
		rels[i].GUID = fmt.Sprintf("sv22-%d", i)
	}
	h.fake.setReleases(seasonKey(1, 22), rels...)
	cands := sweep.SeasonCandidates(rels, nil)
	sweep.Rank(cands)
	var guids []string
	for _, c := range cands {
		guids = append(guids, c.GUID)
	}
	return h, guids
}

func (h *harness) advance(d time.Duration) {
	h.clock.mu.Lock()
	defer h.clock.mu.Unlock()
	h.clock.t = h.clock.t.Add(d)
}

func (h *harness) refusedRows(t *testing.T) []db.SweepRefused {
	t.Helper()
	rows, err := h.db.ListSweepRefused(context.Background(), t0().Add(-365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestS5_1_TimeoutRefusedSevenDays(t *testing.T) {
	t.Parallel()
	h, guids := refusalHarness(t)
	h.fake.beforePost = func() { panic(http.ErrAbortHandler) }
	if got := itemResults(h.run(t, false, settings())); !slices.Contains(got, "refused") {
		t.Fatalf("run 1 results = %v, want a refusal", got)
	}
	rows := h.refusedRows(t)
	if len(rows) == 0 {
		t.Fatal("no refusal stored")
	}
	for _, r := range rows {
		if r.Code != sweepstate.OtherRefusal {
			t.Errorf("code = %q for %q, want other", r.Code, r.Reason)
		}
	}
	h.fake.beforePost = nil
	posted := h.fake.countWrites("POST /release " + guids[0])
	h.advance(6 * 24 * time.Hour)
	if got := itemResults(h.run(t, false, settings())); !slices.Equal(got, []string{"no_pack"}) {
		t.Errorf("run 2 results = %v, want the searched season skipped as no_pack", got)
	}
	if n := h.fake.countWrites("POST /release " + guids[0]); n != posted {
		t.Errorf("first candidate posted %d times, want %d: still blocked at 6 days", n, posted)
	}
	h.advance(2 * 24 * time.Hour)
	h.run(t, false, settings())
	if n := h.fake.countWrites("POST /release " + guids[0]); n <= posted {
		t.Errorf("first candidate posted %d times, want more than %d after 7 days", n, posted)
	}
}

func TestS5_2_TorrentBlockedStoredAsCode(t *testing.T) {
	t.Parallel()
	h, guids := refusalHarness(t)
	h.fake.plan(guids[0], grabPlan{status: http.StatusInternalServerError, body: `{"code": "torrent_blocked"}`})
	h.run(t, false, settings())
	rows := h.refusedRows(t)
	if len(rows) != 1 || rows[0].Code != sweepstate.TorrentBlocked {
		t.Fatalf("refused = %+v, want one torrent_blocked", rows)
	}
	h.advance(30 * 24 * time.Hour)
	h.run(t, false, settings())
	if n := h.fake.countWrites("POST /release " + guids[0]); n != 1 {
		t.Errorf("blocked release posted %d times, want 1 (blocked for 90 days)", n)
	}
}

func TestS5_6_ProviderErrorGrabbedNextRun(t *testing.T) {
	t.Parallel()
	h, guids := refusalHarness(t)
	for _, g := range guids {
		h.fake.plan(g, grabPlan{status: http.StatusInternalServerError, body: `{"code": "provider_error"}`})
	}
	if got := itemResults(h.run(t, false, settings())); !slices.Contains(got, "refused") {
		t.Fatalf("run 1 results = %v, want a refusal", got)
	}
	for _, r := range h.refusedRows(t) {
		if r.Code != sweepstate.ProviderError {
			t.Errorf("code = %q, want provider_error", r.Code)
		}
	}
	h.advance(7 * 24 * time.Hour)
	h.fake.plan(guids[0], grabPlan{grabbed: true, outcome: "downloadFolderImported", after: 1})
	h.run(t, false, settings())
	if n := h.fake.countWrites("POST /release " + guids[0]); n != 2 {
		t.Errorf("POST count = %d, want 2 (provider_error is retried next run); writes %v", n, h.fake.writeLog())
	}
}

func TestS5_7_PruneFailureStopsRunStart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	err := h.db.AddSweepRefused(ctx, &db.SweepRefused{TitleKey: "old", ReleaseTitle: "Old.S01", Indexer: "Comet",
		RefusedAt: t0().Add(-30 * 24 * time.Hour), Reason: "500 x", Code: sweepstate.OtherRefusal})
	if err != nil {
		t.Fatal(err)
	}
	h.rawExec(t, blockRefusedDeleteSQL)
	id, err := h.runner.Run(ctx, "manual", false, settings())
	if err == nil || id != 0 {
		t.Errorf("Run = %d, %v; want the prune error and no run", id, err)
	}
	if runs := h.runs(t); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
}
