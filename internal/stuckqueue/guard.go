package stuckqueue

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/minz1/mediafixer/internal/client"
)

var ErrRefused = errors.New("refused")

type target struct {
	movie bool
	id    int
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

func ImportGuard(d Download, cands []client.ImportCandidate, queue []client.QueueEntry) error {
	if len(cands) == 0 {
		return refuse("Sonarr found no files to import")
	}
	got := map[target]bool{}
	for _, c := range cands {
		ts, hasFile := candidateTargets(c)
		if len(ts) == 0 {
			return refuse("%s is not matched to any episode", c.Path)
		}
		if hasFile {
			return refuse("a target of %s already has a file", c.Path)
		}
		for _, t := range ts {
			if got[t] {
				return refuse("more than one file targets the same episode")
			}
			got[t] = true
		}
	}
	want := map[target]bool{}
	for _, r := range d.Records {
		want[recordTarget(r)] = true
	}
	if !maps.Equal(got, want) {
		return refuse("the import maps to different episodes than the queue shows (%s)", d.Targets())
	}
	for _, r := range queue {
		if !strings.EqualFold(r.DownloadID, d.DownloadID) && want[recordTarget(r)] {
			return refuse("another download is queued for %s", d.Targets())
		}
	}
	return nil
}

func candidateTargets(c client.ImportCandidate) ([]target, bool) {
	if c.MovieID != 0 {
		return []target{{movie: true, id: c.MovieID}}, c.MovieHasFile
	}
	ts := make([]target, 0, len(c.Episodes))
	hasFile := false
	for _, e := range c.Episodes {
		ts = append(ts, target{id: e.ID})
		hasFile = hasFile || e.HasFile
	}
	return ts, hasFile
}

func recordTarget(r client.QueueEntry) target {
	if r.MovieID != 0 {
		return target{movie: true, id: r.MovieID}
	}
	return target{id: r.EpisodeID}
}
