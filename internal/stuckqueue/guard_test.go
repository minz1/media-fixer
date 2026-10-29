package stuckqueue_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func novaDownload() stuckqueue.Download {
	r := stuckRec(1886296760, "NOVA", 14607, false, "Unable to determine if file is a sample")
	r.SeasonNumber, r.EpisodeNumber = 9, 1
	return stuckqueue.Group(stuckqueue.AppSonarr, []client.QueueEntry{r})[0]
}

func novaCandidate(epID int, hasFile bool) client.ImportCandidate {
	return client.ImportCandidate{
		Path: "/data/downloads/sonarr/NOVA/nova.mkv", DownloadID: "NOVA", SeriesID: 188, SeasonNumber: 9,
		Episodes:   []client.CandidateEpisode{{ID: epID, SeasonNumber: 9, EpisodeNumber: 1, HasFile: hasFile}},
		Rejections: []string{"Unable to determine if file is a sample"},
	}
}

func wantRefused(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, stuckqueue.ErrRefused) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("err = %v, want refusal containing %q", err, reason)
	}
}

func TestImportGuard_OK(t *testing.T) {
	t.Parallel()
	d := novaDownload()
	if err := stuckqueue.ImportGuard(d, []client.ImportCandidate{novaCandidate(14607, false)}, d.Records); err != nil {
		t.Fatal(err)
	}
}

func TestImportGuard_TargetHasFile(t *testing.T) {
	t.Parallel()
	d := novaDownload()
	wantRefused(t, stuckqueue.ImportGuard(d, []client.ImportCandidate{novaCandidate(14607, true)}, d.Records),
		"already has a file")
}

func TestImportGuard_TwoFilesOneEpisode(t *testing.T) {
	t.Parallel()
	d := novaDownload()
	cands := []client.ImportCandidate{novaCandidate(14607, false), novaCandidate(14607, false)}
	wantRefused(t, stuckqueue.ImportGuard(d, cands, d.Records), "more than one file")
}

func TestImportGuard_DifferentEpisodes(t *testing.T) {
	t.Parallel()
	d := novaDownload()
	wantRefused(t, stuckqueue.ImportGuard(d, []client.ImportCandidate{novaCandidate(10908, false)}, d.Records),
		"maps to different episodes")
}

func TestImportGuard_NotMatched(t *testing.T) {
	t.Parallel()
	d := novaDownload()
	c := novaCandidate(14607, false)
	c.Episodes = nil
	wantRefused(t, stuckqueue.ImportGuard(d, []client.ImportCandidate{c}, d.Records), "not matched")
}

func TestImportGuard_AnotherDownload(t *testing.T) {
	t.Parallel()
	d := novaDownload()
	other := stuckRec(5, "OTHER", 14607, false)
	other.Status, other.TrackedDownloadState = "downloading", "downloading"
	queue := append(append([]client.QueueEntry(nil), d.Records...), other)
	wantRefused(t, stuckqueue.ImportGuard(d, []client.ImportCandidate{novaCandidate(14607, false)}, queue),
		"another download")
}

func TestImportGuard_NoFiles(t *testing.T) {
	t.Parallel()
	d := novaDownload()
	wantRefused(t, stuckqueue.ImportGuard(d, nil, d.Records), "no files")
}

func TestImportGuard_Radarr(t *testing.T) {
	t.Parallel()
	r := client.QueueEntry{
		ID: 1, DownloadID: "M", Status: "completed", TrackedDownloadState: "importPending",
		MovieID: 5, HasFileKnown: true,
	}
	d := stuckqueue.Group(stuckqueue.AppRadarr, []client.QueueEntry{r})[0]
	c := client.ImportCandidate{Path: "/m.mkv", DownloadID: "M", MovieID: 5}
	if err := stuckqueue.ImportGuard(d, []client.ImportCandidate{c}, d.Records); err != nil {
		t.Fatal(err)
	}
	c.MovieHasFile = true
	wantRefused(t, stuckqueue.ImportGuard(d, []client.ImportCandidate{c}, d.Records), "already has a file")
}
