package stuckqueue_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func mapEps() []client.Episode {
	return []client.Episode{
		{SeriesID: 7, ID: 1, SeasonNumber: 1, EpisodeNumber: 1},
		{SeriesID: 7, ID: 2, SeasonNumber: 1, EpisodeNumber: 2},
		{SeriesID: 7, ID: 3, SeasonNumber: 2, EpisodeNumber: 1},
		{SeriesID: 7, ID: 5, SeasonNumber: 1, EpisodeNumber: 5, HasFile: true},
		{SeriesID: 7, ID: 6, SeasonNumber: 1, EpisodeNumber: 6},
		{SeriesID: 7, ID: 90, SeasonNumber: 0, EpisodeNumber: 1},
		{SeriesID: 7, ID: 91, SeasonNumber: 0, EpisodeNumber: 2},
	}
}

func mapCands() []client.ImportCandidate {
	var out []client.ImportCandidate
	for _, p := range []string{"/d/a.mkv", "/d/b.mkv", "/d/c.mkv"} {
		out = append(out, client.ImportCandidate{Path: p, SeriesID: 7})
	}
	return out
}

func wantReasons(t *testing.T, rs []stuckqueue.RowResult, want ...string) {
	t.Helper()
	if got := reasons(rs); !slices.Equal(got, want) {
		t.Fatalf("reasons = %q, want %q", got, want)
	}
}

func wantRefusal(t *testing.T, err error, sub string) {
	t.Helper()
	if !errors.Is(err, stuckqueue.ErrRefused) || !strings.Contains(err.Error(), sub) {
		t.Fatalf("err = %v, want refusal containing %q", err, sub)
	}
}

func reasons(rs []stuckqueue.RowResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Reason
	}
	return out
}

func check(rows []stuckqueue.MappedFile, queue []client.QueueEntry) ([]stuckqueue.RowResult, error) {
	return stuckqueue.CheckMapping(7, rows, mapCands(), mapEps(), queue, "A")
}

func TestCheckMapping_OK(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{
		{Path: "/d/a.mkv", EpisodeIDs: []int{1, 2}},
		{Path: "/d/b.mkv", EpisodeIDs: []int{3}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantReasons(t, rs, "", "")
	if rs[0].Season != 1 {
		t.Fatalf("season = %d, want 1", rs[0].Season)
	}
	if rs[1].Season != 2 {
		t.Fatalf("season = %d, want 2", rs[1].Season)
	}
}

func TestCheckMapping_NoRows(t *testing.T) {
	t.Parallel()
	_, err := check(nil, nil)
	wantRefusal(t, err, "nothing to import")
}

func TestCheckMapping_ForeignPath(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{{Path: "/other/z.mkv", EpisodeIDs: []int{1}}}, nil)
	wantRefusal(t, err, "z.mkv")
	wantReasons(t, rs, "not a file of this download")
}

func TestCheckMapping_DuplicatePath(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{
		{Path: "/d/a.mkv", EpisodeIDs: []int{1}},
		{Path: "/d/a.mkv", EpisodeIDs: []int{2}},
	}, nil)
	wantRefusal(t, err, "")
	wantReasons(t, rs, "", "file listed twice")
}

func TestCheckMapping_NoEpisodes(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{{Path: "/d/a.mkv"}}, nil)
	wantRefusal(t, err, "")
	wantReasons(t, rs, "no episodes")
}

func TestCheckMapping_ForeignEpisode(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{{Path: "/d/a.mkv", EpisodeIDs: []int{404}}}, nil)
	wantRefusal(t, err, "")
	wantReasons(t, rs, "episode 404 is not in this show")
}

func TestCheckMapping_TwoSeasonsInOneRow(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{{Path: "/d/a.mkv", EpisodeIDs: []int{1, 3}}}, nil)
	wantRefusal(t, err, "")
	wantReasons(t, rs, "episodes from more than one season")
}

func TestCheckMapping_SpecialsRow(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{{Path: "/d/a.mkv", EpisodeIDs: []int{90, 91}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Season != 0 {
		t.Fatalf("season = %d, want 0", rs[0].Season)
	}
}

func TestCheckMapping_EpisodeTwice(t *testing.T) {
	t.Parallel()
	rs, err := check([]stuckqueue.MappedFile{
		{Path: "/d/a.mkv", EpisodeIDs: []int{1}},
		{Path: "/d/b.mkv", EpisodeIDs: []int{1}},
	}, nil)
	wantRefusal(t, err, "")
	wantReasons(t, rs, "", "S01E01 is targeted twice")
}

func TestCheckMapping_HasFileFromEpisodes(t *testing.T) {
	t.Parallel()
	cands := mapCands()
	cands[0].Episodes = []client.CandidateEpisode{{ID: 5, HasFile: false}}
	rs, err := stuckqueue.CheckMapping(
		7,
		[]stuckqueue.MappedFile{{Path: "/d/a.mkv", EpisodeIDs: []int{5}}},
		cands,
		mapEps(),
		nil,
		"A",
	)
	wantRefusal(t, err, "")
	wantReasons(t, rs, "S01E05 already has a file")
}

func TestCheckMapping_OtherDownloadQueued(t *testing.T) {
	t.Parallel()
	queue := []client.QueueEntry{
		{DownloadID: "a", EpisodeID: 1},
		{DownloadID: "B", EpisodeID: 6},
	}
	rs, err := check([]stuckqueue.MappedFile{
		{Path: "/d/a.mkv", EpisodeIDs: []int{1}},
		{Path: "/d/b.mkv", EpisodeIDs: []int{6}},
	}, queue)
	wantRefusal(t, err, "")
	wantReasons(t, rs, "", "another download is queued for S01E06")
}

func TestDownload_OwnedBy(t *testing.T) {
	t.Parallel()
	d := func(ids ...int) stuckqueue.Download {
		var d stuckqueue.Download
		for _, id := range ids {
			d.Records = append(d.Records, client.QueueEntry{SeriesID: id})
		}
		return d
	}
	if !d(7).OwnedBy(7) {
		t.Fatal("d(7).OwnedBy(7)")
	}
	if !d(7, 0).OwnedBy(7) {
		t.Fatal("d(7, 0).OwnedBy(7)")
	}
	if d(7, 8).OwnedBy(7) {
		t.Fatal("d(7, 8).OwnedBy(7)")
	}
	if d(0).OwnedBy(7) {
		t.Fatal("d(0).OwnedBy(7)")
	}
	if d().OwnedBy(7) {
		t.Fatal("d().OwnedBy(7)")
	}
}
