package stuckqueue_test

import (
	"context"
	"maps"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const (
	notUpgrade = "Not an upgrade for existing episode file(s). Existing quality: WEBDL-1080p"
	notCF      = "Not a Custom Format upgrade for existing episode file(s). New: [10bit]"
	unexpected = "Episode 1x26 was unexpected considering the folder name"
)

func stuckRec(id int, dl string, ep int, hasFile bool, msgs ...string) client.QueueEntry {
	return client.QueueEntry{
		ID: id, DownloadID: dl, Status: "completed", TrackedDownloadState: "importPending",
		SeriesID: 1, EpisodeID: ep, SeasonNumber: 1, EpisodeNumber: ep,
		HasFile: hasFile, HasFileKnown: true, Messages: msgs,
	}
}

func liveDownloads(t *testing.T, app stuckqueue.App, path string) []stuckqueue.Download {
	t.Helper()
	f := newFakeArr(t)
	f.loadFixture(t, path)
	es, err := f.client().QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stuckqueue.Group(app, es)
}

func TestClassify_LiveQueue(t *testing.T) {
	t.Parallel()
	byID := map[int]stuckqueue.Verdict{}
	counts := make(map[stuckqueue.Verdict]int)
	for _, d := range liveDownloads(t, stuckqueue.AppSonarr, "../client/testdata/sonarr-queue-2026-10-29.json") {
		if !d.Stuck() {
			t.Errorf("%s not stuck", d.Title)
		}
		v := stuckqueue.Classify(d)
		counts[v]++
		for _, r := range d.Records {
			byID[r.ID] = v
		}
	}
	want := map[stuckqueue.Verdict]int{
		stuckqueue.VerdictClear: 8, stuckqueue.VerdictGap: 9, stuckqueue.VerdictUnmapped: 1,
	}
	if !maps.Equal(counts, want) {
		t.Errorf("sonarr counts = %v, want %v", counts, want)
	}
	for id, v := range map[int]stuckqueue.Verdict{
		1464435435: stuckqueue.VerdictClear, 233788850: stuckqueue.VerdictGap, 621463217: stuckqueue.VerdictGap,
		1910858291: stuckqueue.VerdictGap, 1304969599: stuckqueue.VerdictUnmapped,
	} {
		if byID[id] != v {
			t.Errorf("record %d = %s, want %s", id, byID[id], v)
		}
	}
	radarr := liveDownloads(t, stuckqueue.AppRadarr, "../client/testdata/radarr-queue-2026-10-29.json")
	if len(radarr) != 1 || stuckqueue.Classify(radarr[0]) != stuckqueue.VerdictUnmapped {
		t.Errorf("radarr = %+v", radarr)
	}
}

func classifyOne(recs ...client.QueueEntry) stuckqueue.Verdict {
	return stuckqueue.Classify(stuckqueue.Group(stuckqueue.AppSonarr, recs)[0])
}

func TestClassify_PackWithOneHole(t *testing.T) {
	t.Parallel()
	if v := classifyOne(stuckRec(1, "D", 1, true, notUpgrade), stuckRec(2, "D", 2, false)); v != stuckqueue.VerdictGap {
		t.Fatalf("verdict = %s, want gap", v)
	}
}

func TestClassify_MixedMessages(t *testing.T) {
	t.Parallel()
	if v := classifyOne(stuckRec(1, "D", 3, true, notCF, unexpected)); v != stuckqueue.VerdictGap {
		t.Fatalf("verdict = %s, want gap", v)
	}
}

func TestClassify_ClearWithSample(t *testing.T) {
	t.Parallel()
	sample := "Unable to determine if file is a sample"
	if v := classifyOne(stuckRec(1, "D", 3, true, sample, notCF)); v != stuckqueue.VerdictClear {
		t.Fatalf("verdict = %s, want clear", v)
	}
	if v := classifyOne(stuckRec(1, "D", 3, true, sample)); v != stuckqueue.VerdictGap {
		t.Fatalf("sample alone = %s, want gap", v)
	}
}

func TestClassify_UnknownMessage(t *testing.T) {
	t.Parallel()
	if v := classifyOne(stuckRec(1, "D", 3, true, "Something new from Sonarr")); v != stuckqueue.VerdictGap {
		t.Fatalf("verdict = %s, want gap", v)
	}
}

func TestClassify_RadarrUnknownHasFile(t *testing.T) {
	t.Parallel()
	r := client.QueueEntry{
		ID: 1, DownloadID: "D", Status: "completed", TrackedDownloadState: "importPending", MovieID: 5,
		Messages: []string{"Not an upgrade for existing movie file"},
	}
	if v := stuckqueue.Classify(
		stuckqueue.Group(stuckqueue.AppRadarr, []client.QueueEntry{r})[0],
	); v != stuckqueue.VerdictGap {
		t.Fatalf("verdict = %s, want gap", v)
	}
}

func TestStuck_SiblingDownloading(t *testing.T) {
	t.Parallel()
	busy := stuckRec(2, "D", 2, false)
	busy.Status, busy.TrackedDownloadState = "downloading", "downloading"
	if stuckqueue.Group(stuckqueue.AppSonarr, []client.QueueEntry{stuckRec(1, "D", 1, true, notUpgrade), busy})[0].Stuck() {
		t.Fatal("a download with a sibling still downloading is not stuck")
	}
}

func TestGroup_DownloadIDCaseInsensitive(t *testing.T) {
	t.Parallel()
	ds := stuckqueue.Group(
		stuckqueue.AppSonarr,
		[]client.QueueEntry{stuckRec(1, "abc", 1, true), stuckRec(2, "ABC", 2, true)},
	)
	if len(ds) != 1 || len(ds[0].Records) != 2 {
		t.Fatalf("groups = %+v", ds)
	}
}

func TestTargets(t *testing.T) {
	t.Parallel()
	a, b := stuckRec(1, "D", 12, false), stuckRec(2, "D", 11, false)
	a.SeasonNumber, b.SeasonNumber = 3, 3
	if got := stuckqueue.Group(stuckqueue.AppSonarr, []client.QueueEntry{a, b})[0].Targets(); got != "S03E11, S03E12" {
		t.Errorf("targets = %q", got)
	}
	m := client.QueueEntry{ID: 9, DownloadID: "M", MovieID: 5}
	if got := stuckqueue.Group(stuckqueue.AppRadarr, []client.QueueEntry{m})[0].Targets(); got != "movie" {
		t.Errorf("radarr targets = %q", got)
	}
}

func TestMaybeHasFile(t *testing.T) {
	t.Parallel()
	r := stuckRec(1, "D", 1, false)
	r.HasFileKnown = false
	if !stuckqueue.Group(stuckqueue.AppSonarr, []client.QueueEntry{r})[0].MaybeHasFile() {
		t.Fatal("an unknown has-file key must count as maybe")
	}
	if stuckqueue.Group(stuckqueue.AppSonarr, []client.QueueEntry{stuckRec(1, "D", 1, false)})[0].MaybeHasFile() {
		t.Fatal("a known missing file is not maybe")
	}
}
