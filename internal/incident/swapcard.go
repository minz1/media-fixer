package incident

import (
	"fmt"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/opexec"
)

const mib = 1 << 20

func swapFile(f client.SwapFile) string {
	parts := []string{f.Path, fmt.Sprintf("%d MB", f.Size/mib)}
	for _, p := range []string{f.ReleaseGroup, f.Quality} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}

func swapEpisodes(p *client.SwapPlan) string {
	codes := make([]string, len(p.Mapping))
	for i, e := range p.Mapping {
		codes[i] = fmt.Sprintf("S%02dE%02d", e.SeasonNumber, e.EpisodeNumber)
	}
	return strings.Join(codes, ", ")
}

func swapProbe(p *client.SwapProbe) string {
	tracks := make([]string, len(p.Subtitles))
	for i, s := range p.Subtitles {
		tracks[i] = fmt.Sprintf("#%d %s %s", s.Index, s.Language, s.Codec)
	}
	return fmt.Sprintf("%s · wanted #%d · no unknown-codec tracks", strings.Join(tracks, ", "), p.Wanted.Index)
}

func SwapSections(p *client.SwapPlan) []card.Field {
	code := fmt.Sprintf("S%02dE%02d", p.Season, p.Episode)
	episode := fmt.Sprintf("%s %s", p.Title, code)
	if p.EpisodeTitle != "" {
		episode += fmt.Sprintf(" %q", p.EpisodeTitle)
	}
	current := []string{swapFile(p.Current.SwapFile)}
	if p.Current.SourceTitle != "" {
		current = append(current, "grab "+p.Current.SourceTitle)
	}
	if !p.Current.GrabbedAt.IsZero() {
		current = append(current, p.Current.GrabbedAt.Format(time.DateOnly))
	}
	if p.Current.DownloadID != "" {
		current = append(current, "id "+p.Current.DownloadID)
	}
	current = append(current, fmt.Sprintf("%d episode(s)", p.Current.PackSize))
	deletes := "Sonarr deletes this file as part of the import · no recycle bin"
	if p.RecycleBin != "" {
		deletes = "Sonarr deletes this file as part of the import · recycle bin " + p.RecycleBin
	}
	c := p.Candidate
	secs := []card.Field{
		{Name: "Episode", Value: fmt.Sprintf("%s · wants %s subtitles", episode, p.Language)},
		{Name: "Current", Value: strings.Join(current, " · ")},
		{Name: "Delete", Value: deletes},
		{Name: "Candidate", Value: fmt.Sprintf(
			"%s · score %d · grabbed %dh ago of %dh grace", swapFile(c.SwapFile), c.CustomFormatScore,
			c.GrabAgeHours, c.GraceHours,
		)},
	}
	if len(c.Rejections) > 0 {
		secs = append(secs, card.Field{Name: "Rejections", Value: strings.Join(c.Rejections, "; ")})
	}
	blocklist := fmt.Sprintf("Mark grab %s failed (blocklists it)", p.Blocklist.SourceTitle)
	switch {
	case p.Blocklist.Skip:
		blocklist = "Blocklist skipped: " + p.Blocklist.Note
	case p.AutoRedownloadOn:
		blocklist += "; Sonarr then runs an automatic search for " + code
	}
	return append(
		secs,
		card.Field{Name: "Mapping", Value: fmt.Sprintf("%s · guard %s", swapEpisodes(p), p.GuardVerdict)},
		card.Field{Name: "Probe", Value: swapProbe(&p.Probe)},
		card.Field{Name: "Jellyfin", Value: "The episode gets a new Jellyfin item; media-fixer re-points the " +
			"incident after the swap"},
		card.Field{Name: "Blocklist", Value: blocklist},
		card.Field{
			Name:  "Rollback",
			Value: "Not automatic; a re-grab from Sonarr's interactive search is the way back",
		},
	)
}

func swapPartial(r opexec.SwapResult) string {
	if !r.Imported {
		return "Partial: import not confirmed"
	}
	out := "Partial: imported, Jellyfin not told"
	if r.Notified {
		out = "Partial: imported, Jellyfin told"
	}
	if r.Blocklisted {
		out += ", grab blocklisted"
	}
	return out
}
