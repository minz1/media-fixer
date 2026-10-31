package babysit_test

import (
	"context"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/babysit"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

func sampleInput(nHist int) babysit.BundleInput {
	season := 1
	in := babysit.BundleInput{
		Series: client.Series{
			ID:         7,
			Title:      "Seven",
			SeriesType: "standard",
			Monitored:  true,
			Added:      now0().AddDate(-1, 0, 0),
		},
		Seasons: []babysit.SeasonCounts{
			{Season: 1, Aired: 10, OnDisk: 8, Missing: 2},
			{Season: 2, Aired: 10, OnDisk: 10},
		},
		Targets: []db.SweepTarget{
			{
				Target:       "s:7:1",
				SeriesID:     7,
				Season:       1,
				Streak:       3,
				Missing:      2,
				Attempts:     5,
				LastAttempt:  now0(),
				NextEligible: now0(),
			},
			{
				Target:       "s:7:2",
				SeriesID:     7,
				Season:       2,
				Streak:       1,
				Missing:      1,
				Attempts:     1,
				LastAttempt:  now0(),
				NextEligible: now0(),
			},
		},
	}
	for i := range 50 {
		in.Items = append(in.Items, db.SweepItem{
			ID: int64(
				i + 1,
			),
			SeriesID:     7,
			Season:       &season,
			ReleaseTitle: "Seven.S01.Complete.1080p.WEB-DL.group" + strconv.Itoa(i),
			Indexer:      "Nyaa",
			Result:       "failed",
			At:           now0().Add(-time.Duration(i) * time.Hour),
		})
	}
	for i := range nHist {
		in.History = append(in.History, client.SeriesEvent{
			ID: i + 1, EventType: "downloadFailed", Date: now0().Add(-time.Duration(i) * time.Minute),
			SourceTitle: "Seven.S01E0" + strconv.Itoa(i%9) + ".1080p.WEB-DL.group",
			Episode:     &client.HistoryEpisode{SeasonNumber: 1, EpisodeNumber: i%9 + 1},
			Indexer:     "Nyaa", Message: "Download client reported the download as failed",
		})
	}
	return in
}

func shuffled(in babysit.BundleInput) babysit.BundleInput {
	out := in
	out.Items = append([]db.SweepItem(nil), in.Items...)
	out.History = append([]client.SeriesEvent(nil), in.History...)
	out.Targets = append([]db.SweepTarget(nil), in.Targets...)
	out.Seasons = append([]babysit.SeasonCounts(nil), in.Seasons...)
	rand.Shuffle(len(out.Items), func(i, j int) { out.Items[i], out.Items[j] = out.Items[j], out.Items[i] })
	rand.Shuffle(len(out.History), func(i, j int) { out.History[i], out.History[j] = out.History[j], out.History[i] })
	rand.Shuffle(len(out.Targets), func(i, j int) { out.Targets[i], out.Targets[j] = out.Targets[j], out.Targets[i] })
	rand.Shuffle(len(out.Seasons), func(i, j int) { out.Seasons[i], out.Seasons[j] = out.Seasons[j], out.Seasons[i] })
	return out
}

func TestBundleDeterministic(t *testing.T) {
	t.Parallel()
	in := sampleInput(120)
	a := babysit.BuildFacts(in)
	if b := babysit.BuildFacts(shuffled(in)); a != b {
		t.Fatal("bundle depends on input order")
	}
	t.Logf("fixture bundle %d bytes (120 history records, 50 items)", len(a))
	full := babysit.BuildFacts(sampleInput(200))
	t.Logf("worst-case fixture bundle %d bytes (200 history records, 50 items)", len(full))
}

func TestFingerprintChanges(t *testing.T) {
	t.Parallel()
	in := sampleInput(5)
	base := babysit.Fingerprint("m", babysit.BuildFacts(in))
	if babysit.Fingerprint("m", babysit.BuildFacts(in)) != base {
		t.Fatal("not stable")
	}
	if babysit.Fingerprint("m2", babysit.BuildFacts(in)) == base {
		t.Fatal("model ignored")
	}
	more := sampleInput(6)
	if babysit.Fingerprint("m", babysit.BuildFacts(more)) == base {
		t.Fatal("new history record ignored")
	}
	streak := sampleInput(5)
	streak.Targets[0].Streak = 4
	if babysit.Fingerprint("m", babysit.BuildFacts(streak)) == base {
		t.Fatal("streak change ignored")
	}
}

func TestBundleHistoryCaps(t *testing.T) {
	t.Parallel()
	facts := babysit.BuildFacts(sampleInput(300))
	if n := strings.Count(facts, "downloadFailed"); n != 200 {
		t.Fatalf("history lines = %d, want 200", n)
	}
	if !strings.Contains(facts, "100 older history records left out") {
		t.Fatal("omitted line missing")
	}

	big := sampleInput(200)
	for i := range big.History {
		big.History[i].Message = strings.Repeat("x", 1000)
	}
	facts = babysit.BuildFacts(big)
	if len(facts) > 64<<10 {
		t.Fatalf("bundle %d bytes over cap", len(facts))
	}
	kept := strings.Count(facts, "downloadFailed")
	if kept >= 200 || !strings.Contains(facts, strconv.Itoa(200-kept)+" older history records left out") {
		t.Fatalf("kept %d, omitted line wrong", kept)
	}
	newest := big.History[0].SourceTitle
	if !strings.Contains(facts, newest) {
		t.Fatal("newest record dropped")
	}
	last := big.History[199]
	if strings.Contains(facts, last.Date.Format(time.RFC3339)+" | downloadFailed") {
		t.Fatal("oldest record kept while others dropped")
	}
}

func TestBundleRecordWithoutEpisodeOrData(t *testing.T) {
	t.Parallel()
	in := sampleInput(0)
	in.History = []client.SeriesEvent{{
		ID: 1, EventType: "seriesFolderImported", Date: now0(), SourceTitle: "Seven",
	}}
	facts := babysit.BuildFacts(in)
	if !strings.Contains(facts, "seriesFolderImported") || !strings.Contains(facts, "season= episode=") {
		t.Fatalf("record not rendered:\n%s", facts)
	}
}

func TestExplainDryItemsAndQueueOutOfFingerprint(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.stuckShow()
	e.item(e.run(true), 7, "Seven", "DRYONLY.release", now0())
	e.item(e.run(false), 7, "Seven", "LIVE.release", now0())
	if _, err := e.svc.Explain(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.llm.user, "DRYONLY") || !strings.Contains(e.llm.user, "LIVE.release") {
		t.Fatalf("items in bundle wrong:\n%s", e.llm.user)
	}
	e.state.Apps = append(e.state.Apps, stuckqueueApp(queueItem(7, "changed", "msg")))
	r, err := e.svc.Explain(context.Background(), 7)
	if err != nil || !r.FromCache || e.llm.count() != 1 {
		t.Fatalf("queue change invalidated cache: %+v %v calls=%d", r, err, e.llm.count())
	}
	e.target(7, "Seven", 4, 2)
	r, err = e.svc.Explain(context.Background(), 7)
	if err != nil || r.FromCache || e.llm.count() != 2 {
		t.Fatalf("streak change kept cache: %+v %v", r, err)
	}
}
