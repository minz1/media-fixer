package stuckqueue

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/minz1/mediafixer/internal/client"
)

type App string

const (
	AppSonarr App = "sonarr"
	AppRadarr App = "radarr"
)

type Verdict string

const (
	VerdictClear    Verdict = "clear"
	VerdictGap      Verdict = "gap"
	VerdictUnmapped Verdict = "unmapped"
)

const (
	statusCompleted = "completed"
	targetMovie     = "movie"
)

type Download struct {
	App        App
	DownloadID string
	Title      string
	Records    []client.QueueEntry
}

func Group(app App, entries []client.QueueEntry) []Download {
	var out []Download
	idx := map[string]int{}
	for _, e := range entries {
		key := strings.ToLower(e.DownloadID)
		i, ok := idx[key]
		if !ok {
			i = len(out)
			idx[key] = i
			out = append(out, Download{App: app, DownloadID: e.DownloadID, Title: e.Title})
		}
		out[i].Records = append(out[i].Records, e)
	}
	return out
}

func (d Download) Stuck() bool {
	return len(d.Records) > 0 && !slices.ContainsFunc(d.Records, func(r client.QueueEntry) bool {
		return r.Status != statusCompleted ||
			(r.TrackedDownloadState != "importPending" && r.TrackedDownloadState != "importBlocked")
	})
}

func (d Download) MaybeHasFile() bool {
	return slices.ContainsFunc(d.Records, func(r client.QueueEntry) bool { return r.HasFile || !r.HasFileKnown })
}

func (d Download) Targets() string { return targets(d.Records) }

func Classify(d Download) Verdict {
	if slices.ContainsFunc(d.Records, unmapped) {
		return VerdictUnmapped
	}
	allHaveFiles := !slices.ContainsFunc(
		d.Records,
		func(r client.QueueEntry) bool { return !r.HasFileKnown || !r.HasFile },
	)
	notUpgrade, otherMessage := false, false
	for _, r := range d.Records {
		for _, m := range r.Messages {
			switch {
			case hasAnyPrefix(m, notUpgradePrefixes()):
				notUpgrade = true
			case strings.HasPrefix(m, samplePrefix):
			default:
				otherMessage = true
			}
		}
	}
	if allHaveFiles && notUpgrade && !otherMessage {
		return VerdictClear
	}
	return VerdictGap
}

const samplePrefix = "Unable to determine if file is a sample"

func notUpgradePrefixes() []string {
	return []string{"Not an upgrade", "Not a Custom Format upgrade", "Not a quality revision upgrade"}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}

func unmapped(r client.QueueEntry) bool { return r.EpisodeID == 0 && r.MovieID == 0 }

func targets(recs []client.QueueEntry) string {
	if slices.ContainsFunc(recs, func(r client.QueueEntry) bool { return r.MovieID != 0 }) {
		return targetMovie
	}
	eps := slices.DeleteFunc(slices.Clone(recs), unmapped)
	slices.SortFunc(eps, func(a, b client.QueueEntry) int {
		return cmp.Or(cmp.Compare(a.SeasonNumber, b.SeasonNumber), cmp.Compare(a.EpisodeNumber, b.EpisodeNumber))
	})
	parts := make([]string, 0, len(eps))
	for _, r := range eps {
		parts = append(parts, fmt.Sprintf("S%02dE%02d", r.SeasonNumber, r.EpisodeNumber))
	}
	return strings.Join(parts, ", ")
}
