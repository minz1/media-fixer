package sweep_test

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/sweep"
)

var seasonOK = regexp.MustCompile(
	`^(Existing file on disk (is of equal or higher preference|has a equal or higher Custom Format score)|Release in queue is of equal or higher preference)`,
)

func loadReleases(t *testing.T, name string) []client.Release {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(name, ".gz") {
		gz, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			t.Fatal(gzErr)
		}
		r = gz
	}
	var rels []client.Release
	if err = json.NewDecoder(r).Decode(&rels); err != nil {
		t.Fatal(err)
	}
	return rels
}

func mappedIDs(rels []client.Release) map[int]bool {
	ids := map[int]bool{}
	for _, r := range rels {
		for _, m := range r.MappedEpisodeInfo {
			ids[m.ID] = true
		}
	}
	return ids
}

func titles(rels []client.Release) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = r.Title
	}
	return out
}

func byTitle(t *testing.T, rels []client.Release, sub string) client.Release {
	t.Helper()
	for _, r := range rels {
		if strings.Contains(r.Title, sub) {
			return r
		}
	}
	t.Fatalf("no release with %q", sub)
	return client.Release{}
}

func TestSeasonCandidates_SV22(t *testing.T) {
	t.Parallel()
	c := sweep.SeasonCandidates(loadReleases(t, "rel-sv-22.json.gz"), nil)
	if len(c) == 0 {
		t.Fatal("no candidates")
	}
	sweep.Rank(c)
	if !strings.HasPrefix(c[0].Indexer, "Comet") || c[0].ReleaseWeight != 32 {
		t.Fatalf("first = %s w=%d %q", c[0].Indexer, c[0].ReleaseWeight, c[0].Title)
	}
}

func TestSeasonCandidates_SV8(t *testing.T) {
	t.Parallel()
	if c := sweep.SeasonCandidates(loadReleases(t, "rel-sv-8.json.gz"), nil); len(c) != 0 {
		t.Fatalf("candidates = %v, want none", titles(c))
	}
}

func TestSeasonCandidates_AT_S3(t *testing.T) {
	t.Parallel()
	c := sweep.SeasonCandidates(loadReleases(t, "rel-at-s3.json.gz"), nil)
	if len(c) == 0 {
		t.Fatal("no candidates")
	}
	for _, r := range c {
		if !r.FullSeason {
			t.Errorf("not a full season: %q", r.Title)
		}
		for _, rej := range r.Rejections {
			if !seasonOK.MatchString(rej) {
				t.Errorf("%q has rejection %q", r.Title, rej)
			}
		}
	}
	sweep.Rank(c)
	if strings.Contains(c[0].Quality.Quality.Name, "480") {
		t.Fatalf("first = %q (%s)", c[0].Title, c[0].Quality.Quality.Name)
	}
}

func TestAnimeCandidates_OP(t *testing.T) {
	t.Parallel()
	rels := loadReleases(t, "rel-op.json.gz")
	c := sweep.AnimeCandidates(rels, mappedIDs(rels), nil)
	if len(c) != 1 || !strings.Contains(c[0].Title, "601-700") {
		t.Fatalf("candidates = %v, want only the 601-700 batch", titles(c))
	}
}

func TestAnimeCandidates_OP19(t *testing.T) {
	t.Parallel()
	rels := loadReleases(t, "rel-op19.json.gz")
	got := strings.Join(titles(sweep.AnimeCandidates(rels, mappedIDs(rels), nil)), "\n")
	for _, s := range []string{"S07", "S22", "S04", "S01"} {
		if !strings.Contains(got, s) {
			t.Errorf("no %s pack in\n%s", s, got)
		}
	}
	s07 := byTitle(t, rels, "One.Piece.1999.S07.REPACK")
	c := sweep.AnimeCandidates(rels, mappedIDs([]client.Release{s07}), nil)
	if len(c) != 1 || c[0].Title != s07.Title {
		t.Fatalf("season 7 only: candidates = %v", titles(c))
	}
}

func TestAnimeCandidates_RejectsMovies(t *testing.T) {
	t.Parallel()
	rels := loadReleases(t, "synthetic.json")
	movies := byTitle(t, rels, "Movies 01-13")
	if c := sweep.AnimeCandidates(rels, mappedIDs([]client.Release{movies}), nil); len(c) != 0 {
		t.Fatalf("candidates = %v, want none", titles(c))
	}
}

func TestAnimeCandidates_AcceptsNyaaBatch(t *testing.T) {
	t.Parallel()
	rels := loadReleases(t, "synthetic.json")
	batch := byTitle(t, rels, "701~800")
	c := sweep.AnimeCandidates(rels, mappedIDs([]client.Release{batch}), nil)
	if len(c) != 1 || c[0].Title != batch.Title {
		t.Fatalf("candidates = %v, want the 701~800 batch", titles(c))
	}
	if n := len(sweep.Covers(c[0], mappedIDs([]client.Release{batch}))); n != 100 {
		t.Fatalf("covers %d ids, want 100", n)
	}
}

func TestRefusedTwin(t *testing.T) {
	t.Parallel()
	rels := loadReleases(t, "synthetic.json")
	if c := sweep.SeasonCandidates(rels, nil); len(c) != 2 {
		t.Fatalf("unrefused candidates = %v, want both twins", titles(c))
	}
	refused := map[string]bool{sweep.NormTitle("Show S01 DDP 2 0 1080p"): true}
	if c := sweep.SeasonCandidates(rels, refused); len(c) != 0 {
		t.Fatalf("candidates = %v, want both twins dropped", titles(c))
	}
	if c := sweep.AnimeCandidates(rels, map[int]bool{101: true, 102: true}, refused); len(c) != 0 {
		t.Fatalf("anime candidates = %v, want both twins dropped", titles(c))
	}
}

func TestNormTitle(t *testing.T) {
	t.Parallel()
	a := sweep.NormTitle("Survivor (2000) S22 (1080p AMZN WEB-DL H264 SDR DDP 2 0 English - HONE)")
	b := sweep.NormTitle("Survivor (2000) S22 (1080p AMZN WEB-DL H264 SDR DDP 2.0 English - HONE)")
	if a != b || a != "survivor 2000 s22 1080p amzn web dl h264 sdr ddp 2 0 english hone" {
		t.Fatalf("NormTitle = %q / %q", a, b)
	}
}

func TestRank_TierOrder(t *testing.T) {
	t.Parallel()
	rels := []client.Release{
		{Title: "usenet", Indexer: "NZBgeek (Prowlarr)", Protocol: "usenet"},
		{Title: "plain", Indexer: "1337x (Prowlarr)", Protocol: "torrent"},
		{Title: "comet", Indexer: "Comet (Prowlarr)", Protocol: "torrent", ReleaseWeight: 50},
		{Title: "zilean", Indexer: "Zilean (Prowlarr)", Protocol: "torrent", ReleaseWeight: 10},
		{Title: "plain-heavy", Indexer: "1337x (Prowlarr)", Protocol: "torrent", ReleaseWeight: 9},
	}
	sweep.Rank(rels)
	want := []string{"comet", "plain", "plain-heavy", "zilean", "usenet"}
	if got := titles(rels); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestMissing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	eps := []client.Episode{
		{ID: 1, SeasonNumber: 1, Monitored: true, AirDateUtc: &past},
		{ID: 2, SeasonNumber: 1, Monitored: false, AirDateUtc: &past},
		{ID: 3, SeasonNumber: 1, Monitored: true, HasFile: true, AirDateUtc: &past},
		{ID: 4, SeasonNumber: 0, Monitored: true, AirDateUtc: &past},
		{ID: 5, SeasonNumber: 1, Monitored: true},
		{ID: 6, SeasonNumber: 1, Monitored: true, AirDateUtc: &future},
		{ID: 7, SeasonNumber: 2, Monitored: true, AirDateUtc: &past},
		{ID: 8, SeasonNumber: 2, Monitored: true, AirDateUtc: &past},
	}
	var ids []int
	for _, e := range sweep.Missing(eps, map[int]bool{8: true}, now) {
		ids = append(ids, e.ID)
	}
	if !slices.Equal(ids, []int{1, 7}) {
		t.Fatalf("missing = %v, want [1 7]", ids)
	}
}

func TestOutcome(t *testing.T) {
	t.Parallel()
	ev := func(typ, id string) client.HistoryRecord {
		return client.HistoryRecord{EventType: typ, DownloadID: id}
	}
	cases := []struct {
		name string
		hist []client.HistoryRecord
		want string
	}{
		{"imported", []client.HistoryRecord{ev("grabbed", "A"), ev("downloadFolderImported", "A")}, "imported"},
		{"failed", []client.HistoryRecord{ev("grabbed", "A"), ev("downloadFailed", "A")}, "failed"},
		{"imported wins", []client.HistoryRecord{
			ev("grabbed", "A"), ev("downloadFailed", "A"), ev("downloadFolderImported", "A"),
		}, "imported"},
		{"imported before failed", []client.HistoryRecord{
			ev("downloadFolderImported", "A"), ev("downloadFailed", "A"),
		}, "imported"},
		{"only grabbed", []client.HistoryRecord{ev("grabbed", "A")}, ""},
		{"other download", []client.HistoryRecord{ev("grabbed", "A"), ev("downloadFolderImported", "B")}, ""},
	}
	for _, tc := range cases {
		if got := sweep.Outcome(tc.hist, "A"); got != tc.want {
			t.Errorf("%s: Outcome = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGrabbedDownloadID(t *testing.T) {
	t.Parallel()
	hist := []client.HistoryRecord{
		{EventType: "grabbed", SourceTitle: "Show.S01.1080p-X", DownloadID: "OTHER"},
		{EventType: "downloadFailed", SourceTitle: "Show.S01.1080p", DownloadID: "FAILED"},
		{EventType: "grabbed", SourceTitle: "Show.S01.1080p", DownloadID: "HASH"},
	}
	if got := sweep.GrabbedDownloadID(hist, "Show.S01.1080p"); got != "HASH" {
		t.Fatalf("GrabbedDownloadID = %q, want HASH", got)
	}
	if got := sweep.GrabbedDownloadID(hist, "Show.S02.1080p"); got != "" {
		t.Fatalf("GrabbedDownloadID = %q, want empty", got)
	}
}

func TestQueuedEpisodes(t *testing.T) {
	t.Parallel()
	got := sweep.QueuedEpisodes([]client.QueueDetail{{EpisodeID: 4}, {EpisodeID: 9}, {EpisodeID: 0}})
	if len(got) != 2 || !got[4] || !got[9] {
		t.Fatalf("QueuedEpisodes = %v", got)
	}
}

func queueDetail(state string, msgs ...string) client.QueueDetail {
	d := client.QueueDetail{TrackedDownloadState: state}
	d.StatusMessages = append(d.StatusMessages, struct {
		Messages []string `json:"messages"`
	}{Messages: msgs})
	return d
}

func TestIsArchive(t *testing.T) {
	t.Parallel()
	const msg = "Found archive file, might need to be extracted"
	for _, state := range []string{"importBlocked", "importPending", "downloading", "failedPending"} {
		if !sweep.IsArchive(queueDetail(state, msg)) {
			t.Errorf("state %s: not an archive", state)
		}
	}
	for _, state := range []string{"importing", "imported"} {
		if sweep.IsArchive(queueDetail(state, msg)) {
			t.Errorf("state %s: archive", state)
		}
	}
	if sweep.IsArchive(queueDetail("importBlocked", "No files found are eligible for import")) {
		t.Error("other message counted as archive")
	}
	if sweep.IsArchive(client.QueueDetail{TrackedDownloadState: "importBlocked"}) {
		t.Error("no messages counted as archive")
	}
}

func TestIsLeftover(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		"Not a Custom Format upgrade for existing episode file(s)",
		"Not an upgrade for existing episode file(s)",
	} {
		if !sweep.IsLeftover(queueDetail("importBlocked", msg)) {
			t.Errorf("%q: not a leftover", msg)
		}
		if sweep.IsLeftover(queueDetail("importing", msg)) {
			t.Errorf("%q while importing: leftover", msg)
		}
	}
	if sweep.IsLeftover(queueDetail("importBlocked", "Found archive file, might need to be extracted")) {
		t.Error("archive counted as leftover")
	}
}

func TestAnimeCandidates_AnchorsEveryAlternative(t *testing.T) {
	t.Parallel()
	rels := loadReleases(t, "synthetic.json")
	batch := byTitle(t, rels, "701~800")
	ids := mappedIDs([]client.Release{batch})
	for rej, want := range map[string]int{
		"Full season pack":                  1,
		"Episode wasn't requested":          1,
		"Not a Full season pack":            0,
		"Unknown: Episode wasn't requested": 0,
	} {
		r := batch
		r.Rejections = []string{rej}
		if n := len(sweep.AnimeCandidates([]client.Release{r}, ids, nil)); n != want {
			t.Errorf("rejection %q: %d candidates, want %d", rej, n, want)
		}
	}
}

func TestBlocking(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	rows := []db.SweepRefused{
		{TitleKey: "blocked-30d", RefusedAt: t0().Add(-30 * day), Reason: "500 x code=torrent_blocked"},
		{TitleKey: "blocked-91d", RefusedAt: t0().Add(-91 * day), Reason: "500 x code=torrent_blocked"},
		{TitleKey: "provider-1h", RefusedAt: t0().Add(-time.Hour), Reason: "500 x code=provider_error"},
		{TitleKey: "plain-6d", RefusedAt: t0().Add(-6 * day), Reason: "500 refused"},
		{TitleKey: "plain-8d", RefusedAt: t0().Add(-8 * day), Reason: "500 refused"},
	}
	var got []string
	for _, r := range sweep.Blocking(rows, t0()) {
		got = append(got, r.TitleKey)
	}
	if want := []string{"blocked-30d", "plain-6d"}; !slices.Equal(got, want) {
		t.Errorf("blocking = %v, want %v", got, want)
	}
}
