package sweep

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const seasonAlts = `Existing file on disk (is of equal or higher preference|has a equal or higher Custom Format score)` +
	`|Release in queue is of equal or higher preference`

var (
	seasonOK   = regexp.MustCompile(`^(?:` + seasonAlts + `)`)
	animeOK    = regexp.MustCompile(`^(?:` + seasonAlts + `|Episode wasn't requested|Full season pack)`)
	movieRE    = regexp.MustCompile(`(?i)\b(movies?|film|gekijouban)\b`)
	nonAlnum   = regexp.MustCompile(`[^a-z0-9]+`)
	archiveRE  = regexp.MustCompile(`Found archive file, might need to be extracted`)
	leftoverRE = regexp.MustCompile(`Not (a Custom Format upgrade|an upgrade) for existing episode file\(s\)`)
)

func Missing(eps []client.Episode, queued map[int]bool, now time.Time) []client.Episode {
	var out []client.Episode
	for _, e := range eps {
		if e.Monitored && !e.HasFile && e.SeasonNumber > 0 && e.AirDateUtc != nil &&
			e.AirDateUtc.Before(now) && !queued[e.ID] {
			out = append(out, e)
		}
	}
	return out
}

func SeasonCandidates(rels []client.Release, refused map[string]bool) []client.Release {
	var out []client.Release
	for _, r := range rels {
		if r.FullSeason && allMatch(r.Rejections, seasonOK) && !refused[NormTitle(r.Title)] {
			out = append(out, r)
		}
	}
	return out
}

func AnimeCandidates(rels []client.Release, uncovered map[int]bool, refused map[string]bool) []client.Release {
	var out []client.Release
	for _, r := range rels {
		if !movieRE.MatchString(r.Title) && len(Covers(r, uncovered)) >= 2 && allMatch(r.Rejections, animeOK) &&
			!refused[NormTitle(r.Title)] {
			out = append(out, r)
		}
	}
	return out
}

func Covers(r client.Release, uncovered map[int]bool) []int {
	var ids []int
	for _, m := range r.MappedEpisodeInfo {
		if uncovered[m.ID] {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func Rank(rels []client.Release) {
	slices.SortStableFunc(rels, func(a, b client.Release) int {
		if c := rankTier(a) - rankTier(b); c != 0 {
			return c
		}
		return a.ReleaseWeight - b.ReleaseWeight
	})
}

func rankTier(r client.Release) int {
	tier := 3
	if strings.HasPrefix(r.Indexer, "Comet") || strings.HasPrefix(r.Indexer, "Zilean") {
		tier -= 2
	}
	if r.Protocol == "torrent" {
		tier--
	}
	return tier
}

func Outcome(hist []client.HistoryRecord, downloadID string) string {
	out := ""
	for _, h := range hist {
		if !strings.EqualFold(h.DownloadID, downloadID) {
			continue
		}
		switch h.EventType {
		case "downloadFolderImported":
			return "imported"
		case "downloadFailed":
			out = "failed"
		}
	}
	return out
}

func GrabbedDownloadID(hist []client.HistoryRecord, title string) string {
	id := ""
	for _, h := range hist {
		if h.EventType == "grabbed" && h.SourceTitle == title {
			id = h.DownloadID
		}
	}
	return id
}

func NormTitle(title string) string {
	return strings.TrimSpace(nonAlnum.ReplaceAllString(strings.ToLower(title), " "))
}

func QueuedEpisodes(q []client.QueueDetail) map[int]bool {
	ids := map[int]bool{}
	for _, d := range q {
		if d.EpisodeID != 0 {
			ids[d.EpisodeID] = true
		}
	}
	return ids
}

func IsArchive(d client.QueueDetail) bool {
	return hasMessage(d, archiveRE)
}

func IsLeftover(d client.QueueDetail) bool {
	return hasMessage(d, leftoverRE)
}

func hasMessage(d client.QueueDetail, re *regexp.Regexp) bool {
	if strings.EqualFold(d.TrackedDownloadState, "importing") || strings.EqualFold(d.TrackedDownloadState, "imported") {
		return false
	}
	for _, s := range d.StatusMessages {
		if slices.ContainsFunc(s.Messages, re.MatchString) {
			return true
		}
	}
	return false
}

func allMatch(rejections []string, re *regexp.Regexp) bool {
	for _, r := range rejections {
		if !re.MatchString(r) {
			return false
		}
	}
	return true
}

func Blocking(rows []db.SweepRefused, now time.Time) []db.SweepRefused {
	var out []db.SweepRefused
	for _, r := range rows {
		memory := RefusedMemory
		switch {
		case strings.Contains(r.Reason, client.CodeTorrentBlocked):
			memory = BlockedMemory
		case strings.Contains(r.Reason, client.CodeProviderError):
			continue
		}
		if !r.RefusedAt.Before(now.Add(-memory)) {
			out = append(out, r)
		}
	}
	return out
}
