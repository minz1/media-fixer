package stuckqueue

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/minz1/mediafixer/internal/client"
)

type MappedFile struct {
	Path       string
	EpisodeIDs []int
}

type RowResult struct {
	File   MappedFile
	Season int
	Reason string
}

func CheckMapping(
	seriesID int,
	rows []MappedFile,
	cands []client.ImportCandidate,
	eps []client.Episode,
	queue []client.QueueEntry,
	downloadID string,
) ([]RowResult, error) {
	if len(rows) == 0 {
		return nil, refuse("nothing to import")
	}
	paths := map[string]bool{}
	for _, c := range cands {
		paths[c.Path] = true
	}
	byID := map[int]client.Episode{}
	for _, e := range eps {
		if e.SeriesID == seriesID {
			byID[e.ID] = e
		}
	}
	other := map[int]bool{}
	for _, q := range queue {
		if !strings.EqualFold(q.DownloadID, downloadID) {
			other[q.EpisodeID] = true
		}
	}
	seenPath := map[string]bool{}
	seenEp := map[int]bool{}
	out := make([]RowResult, len(rows))
	var first error
	for i, r := range rows {
		season, reason := checkRow(r, paths, seenPath, seenEp, byID, other)
		out[i] = RowResult{File: r, Season: season, Reason: reason}
		if reason != "" && first == nil {
			first = refuse("%s: %s", filepath.Base(r.Path), reason)
		}
	}
	return out, first
}

func checkRow(
	r MappedFile,
	paths, seenPath map[string]bool,
	seenEp map[int]bool,
	byID map[int]client.Episode,
	other map[int]bool,
) (int, string) {
	if !paths[r.Path] {
		return 0, "not a file of this download"
	}
	if seenPath[r.Path] {
		return 0, "file listed twice"
	}
	seenPath[r.Path] = true
	if len(r.EpisodeIDs) == 0 {
		return 0, "no episodes"
	}
	season := byID[r.EpisodeIDs[0]].SeasonNumber
	for _, id := range r.EpisodeIDs {
		e, ok := byID[id]
		if !ok {
			return 0, fmt.Sprintf("episode %d is not in this show", id)
		}
		if e.SeasonNumber != season {
			return 0, "episodes from more than one season"
		}
	}
	for _, id := range r.EpisodeIDs {
		e := byID[id]
		label := fmt.Sprintf("S%02dE%02d", e.SeasonNumber, e.EpisodeNumber)
		switch {
		case seenEp[id]:
			return 0, label + " is targeted twice"
		case e.HasFile:
			return 0, label + " already has a file"
		case other[id]:
			return 0, "another download is queued for " + label
		}
	}
	for _, id := range r.EpisodeIDs {
		seenEp[id] = true
	}
	return season, ""
}

func (d Download) OwnedBy(seriesID int) bool {
	mine := false
	for _, r := range d.Records {
		switch r.SeriesID {
		case seriesID:
			mine = true
		case 0:
		default:
			return false
		}
	}
	return mine
}
