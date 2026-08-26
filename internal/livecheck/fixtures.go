package livecheck

import (
	"context"
	"encoding/json"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

// Fixtures are the live data samples checks run against — a real series, a
// real movie, a real torrent, a real file path. Discovery runs once per
// Runner.Run and is itself reported (Report.Fixtures), so "no fixture found"
// is visible instead of silently skipping every dependent check.
type Fixtures struct {
	JellyfinItemID   string `json:"jellyfin_item_id,omitempty"`
	JellyfinItemType string `json:"jellyfin_item_type,omitempty"`
	// JellyfinPlaybackItemID is what jellyfin_playback_info is actually
	// called against. On at least some Jellyfin builds, PlaybackInfo on a
	// Series throws (InvalidCastException: Series -> IHasMediaSources)
	// instead of returning empty MediaSources, regardless of whether the
	// series has episodes indexed — confirmed against a live instance whose
	// series had 51 indexed episodes and still 500'd. So when JellyfinItemID
	// is a Series, this is resolved to its first episode instead.
	JellyfinPlaybackItemID string `json:"jellyfin_playback_item_id,omitempty"`
	SeriesTitle            string `json:"series_title,omitempty"`
	MovieTitle             string `json:"movie_title,omitempty"`
	TorrentName            string `json:"torrent_name,omitempty"`
	SamplePath             string `json:"sample_path,omitempty"`
	RepairEntryName        string `json:"repair_entry_name,omitempty"`

	// Notes records why a fixture could not be discovered, so a "degraded"
	// dependent check has an explanation instead of a bare "no fixture".
	Notes []string `json:"notes,omitempty"`
}

// missing records why a fixture couldn't be found.
func (fx *Fixtures) missing(reason string) {
	fx.Notes = append(fx.Notes, reason)
}

// discoverFixtures probes the live services for sample data. override seeds
// (or entirely replaces) any field the caller already knows, e.g. from
// config's [selftest] block — an explicit override always wins over
// discovery for that field.
func discoverFixtures(ctx context.Context, disp *agent.Dispatcher, override Fixtures) Fixtures {
	fx := override

	discoverArrTitles(ctx, disp, &fx)
	discoverJellyfinItem(ctx, disp, &fx)
	discoverTorrentAndSample(ctx, disp, &fx)
	discoverRepairEntry(ctx, disp, &fx)

	return fx
}

func discoverArrTitles(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	if fx.SeriesTitle == "" {
		if disp.Sonarr == nil {
			fx.missing("sonarr not configured")
		} else if series, err := disp.Sonarr.ListSeries(ctx); err != nil {
			fx.missing("sonarr series discovery: " + err.Error())
		} else if len(series) == 0 {
			fx.missing("sonarr has no series")
		} else {
			fx.SeriesTitle = series[0].Title
		}
	}

	if fx.MovieTitle == "" {
		if disp.Radarr == nil {
			fx.missing("radarr not configured")
		} else if movies, err := disp.Radarr.ListMovies(ctx); err != nil {
			fx.missing("radarr movie discovery: " + err.Error())
		} else if len(movies) == 0 {
			fx.missing("radarr has no movies")
		} else {
			fx.MovieTitle = movies[0].Title
		}
	}
}

// discoverJellyfinItem finds a Jellyfin item ID by searching for the
// already-discovered series title, preferring a Series-typed match — this
// deliberately reuses the same SearchItem call the jellyfin_search tool
// makes, so fixture discovery exercises the real lookup path too. It then
// resolves a separate playback-safe item ID (see JellyfinPlaybackItemID).
func discoverJellyfinItem(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	if fx.JellyfinItemID == "" {
		if fx.SeriesTitle == "" {
			return
		}
		items, err := disp.Jellyfin.SearchItem(ctx, fx.SeriesTitle)
		if err != nil {
			fx.missing("jellyfin item discovery: " + err.Error())
			return
		}
		best := items[0]
		for _, item := range items {
			if item.Type == "Series" {
				best = item
				break
			}
		}
		fx.JellyfinItemID = best.ID
		fx.JellyfinItemType = best.Type
	}
	discoverJellyfinPlaybackItem(ctx, disp, fx)
}

// discoverJellyfinPlaybackItem resolves the item ID jellyfin_playback_info is
// actually safe to call: itself if it's not a Series, otherwise its first
// indexed episode.
func discoverJellyfinPlaybackItem(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	if fx.JellyfinPlaybackItemID != "" {
		return
	}
	if fx.JellyfinItemType != "Series" {
		fx.JellyfinPlaybackItemID = fx.JellyfinItemID
		return
	}
	episodes, err := disp.Jellyfin.ListEpisodes(ctx, fx.JellyfinItemID)
	if err != nil {
		fx.missing("jellyfin playback item discovery: " + err.Error())
		return
	}
	if len(episodes) == 0 {
		fx.missing("series has no indexed episodes to test playback info against")
		return
	}
	fx.JellyfinPlaybackItemID = episodes[0].ID
}

// discoverTorrentAndSample finds a torrent name from decypharr, then a
// readable file under it via the media-agent, trying both directory layouts
// this stack has used (see systemPrompt's note that /data/library entries
// are symlinks into /mnt/decypharr/__all__/<torrent>/).
func discoverTorrentAndSample(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	if fx.TorrentName == "" {
		discoverTorrentName(ctx, disp, fx)
	}
	if fx.SamplePath == "" && fx.TorrentName != "" {
		discoverSamplePath(ctx, disp, fx)
	}
}

func discoverTorrentName(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	torrents, err := disp.Decypharr.ListTorrents(ctx, "", "")
	if err != nil {
		fx.missing("decypharr torrent discovery: " + err.Error())
		return
	}
	if len(torrents) == 0 {
		fx.missing("decypharr has no torrents")
		return
	}
	fx.TorrentName = torrents[0].Name
}

// decypharrCandidateDirs are the directory layouts this stack has used for a
// torrent's files, tried in order (see systemPrompt's note that
// /data/library entries are symlinks into /mnt/decypharr/__all__/<torrent>/).
func decypharrCandidateDirs(torrentName string) []string {
	return []string{
		"/mnt/decypharr/" + torrentName,
		"/mnt/decypharr/__all__/" + torrentName,
	}
}

func discoverSamplePath(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	if disp.MediaAgent == nil {
		fx.missing("media-agent not configured, cannot discover sample file")
		return
	}
	for _, dir := range decypharrCandidateDirs(fx.TorrentName) {
		result, err := disp.MediaAgent.ListDirectory(ctx, dir)
		if err != nil || result == nil {
			continue
		}
		if path, ok := firstFilePath(dir, result.Entries); ok {
			fx.SamplePath = path
			return
		}
	}
	fx.missing("no readable file found under torrent directory for " + fx.TorrentName)
}

// firstFilePath returns the first non-directory entry's path, following a
// symlink to its target when present.
func firstFilePath(dir string, entries []mediaagentapi.ListDirEntry) (string, bool) {
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		if entry.IsSymlink && entry.Target != "" {
			return entry.Target, true
		}
		return dir + "/" + entry.Name, true
	}
	return "", false
}

// discoverRepairEntry best-effort extracts an entry name from decypharr's
// repair-health response, whose exact shape isn't documented and has proven
// to shift between decypharr builds — a missing/renamed field here degrades
// only the decypharr_recheck fixture, not the whole run.
func discoverRepairEntry(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	if fx.RepairEntryName != "" {
		return
	}
	raw, err := disp.Decypharr.RepairHealth(ctx)
	if err != nil {
		fx.missing("decypharr repair health discovery: " + err.Error())
		return
	}
	name, ok := firstRepairEntryName(raw)
	if !ok {
		fx.missing("no entry name found in repair health response")
		return
	}
	fx.RepairEntryName = name
}

// firstRepairEntryName best-effort extracts the first entry's name from
// decypharr's repair-health JSON, tolerating a few plausible shapes.
func firstRepairEntryName(raw json.RawMessage) (string, bool) {
	if name, ok := firstRepairEntryNameInArray(raw); ok {
		return name, true
	}
	return firstRepairEntryNameInWrapper(raw)
}

// firstRepairEntryNameInArray handles the shape where the response is a bare
// array of entry objects.
func firstRepairEntryNameInArray(raw json.RawMessage) (string, bool) {
	// Field names tried, in order, for an entry's identifying name across
	// decypharr's various repair-health response shapes. entry_name is the
	// confirmed field on the live minz branch; the others are speculative
	// fallbacks for other builds.
	entryKeys := []string{"entry_name", "name", "entry", "id"}

	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return "", false
	}
	for _, entry := range arr {
		for _, key := range entryKeys {
			if s, ok := entry[key].(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// firstRepairEntryNameInWrapper handles the shape where the response is an
// object wrapping the entry list under one of a few plausible keys.
func firstRepairEntryNameInWrapper(raw json.RawMessage) (string, bool) {
	listKeys := []string{"entries", "health", "items", "results"}

	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return "", false
	}
	for _, key := range listKeys {
		inner, ok := wrapper[key]
		if !ok {
			continue
		}
		if name, found := firstRepairEntryNameInArray(inner); found {
			return name, true
		}
	}
	return "", false
}
