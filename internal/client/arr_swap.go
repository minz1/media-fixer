package client

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/operation"
)

const (
	noteGrabUnknown = "the current file's grab is unknown; blocklist skipped"
	notePackGrab    = "pack grab: blocklist skipped"
	noteOwnGrab     = "the candidate is the current file's own grab; blocklist skipped"
)

type SwapGrab struct {
	HistoryID     int
	DownloadID    string
	GrabbedAt     time.Time
	SourceTitle   string
	EpisodeIDs    []int
	SkipBlocklist bool
	Note          string
}

func (g SwapGrab) WithCandidate(downloadID string) SwapGrab {
	if !g.SkipBlocklist && strings.EqualFold(g.DownloadID, downloadID) {
		g.SkipBlocklist, g.Note = true, noteOwnGrab
	}
	return g
}

func (c *ArrClient) ManualImportFolderCandidates(ctx context.Context, folder string) ([]ImportCandidate, error) {
	return c.manualImportPreview(ctx, url.Values{"folder": {folder}, "filterExistingFiles": {queryFalse}})
}

func (c *ArrClient) GrabBehindFile(ctx context.Context, seriesID, episodeID int, currentPath string) (SwapGrab, error) {
	events, err := c.SeriesHistory(ctx, seriesID)
	if err != nil {
		return SwapGrab{}, err
	}
	unknown := SwapGrab{SkipBlocklist: true, Note: noteGrabUnknown}
	var imported *SeriesEvent
	for i, e := range events {
		if e.EpisodeID == episodeID && e.EventType == "downloadFolderImported" &&
			(imported == nil || e.Date.After(imported.Date)) {
			imported = &events[i]
		}
	}
	if imported == nil || imported.ImportedPath == "" || imported.DownloadID == "" ||
		path.Base(imported.ImportedPath) != path.Base(currentPath) {
		return unknown, nil
	}
	var grab *SeriesEvent
	var ids []int
	for i, e := range events {
		if e.EventType != "grabbed" || !strings.EqualFold(e.DownloadID, imported.DownloadID) {
			continue
		}
		if !slices.Contains(ids, e.EpisodeID) {
			ids = append(ids, e.EpisodeID)
		}
		if e.EpisodeID == episodeID && (grab == nil || e.Date.After(grab.Date)) {
			grab = &events[i]
		}
	}
	if grab == nil {
		return unknown, nil
	}
	slices.Sort(ids)
	g := SwapGrab{
		HistoryID: grab.ID, DownloadID: grab.DownloadID, GrabbedAt: grab.Date,
		SourceTitle: grab.SourceTitle, EpisodeIDs: ids,
	}
	if len(ids) > 1 {
		g.SkipBlocklist, g.Note = true, notePackGrab
	}
	return g, nil
}

func (c *ArrClient) RecycleBin(ctx context.Context) (string, error) {
	var cfg struct {
		RecycleBin string `json:"recycleBin"`
	}
	err := c.do(ctx, c.http, http.MethodGet, "/api/v3/config/mediamanagement", nil, nil, &cfg)
	return cfg.RecycleBin, err
}

type SwapFile struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	ReleaseGroup string `json:"release_group,omitempty"`
	Quality      string `json:"quality,omitempty"`
}

type SwapCurrent struct {
	SwapFile

	FileID      int       `json:"file_id"`
	SourceTitle string    `json:"source_title,omitempty"`
	GrabbedAt   time.Time `json:"grabbed_at,omitzero"`
	DownloadID  string    `json:"download_id,omitempty"`
	PackSize    int       `json:"pack_size"`
}

type SwapCandidate struct {
	SwapFile

	Folder            string    `json:"folder"`
	DownloadID        string    `json:"download_id,omitempty"`
	CustomFormatScore int       `json:"custom_format_score"`
	GrabbedAt         time.Time `json:"grabbed_at,omitzero"`
	GrabAgeHours      int       `json:"grab_age_hours"`
	GraceHours        int       `json:"grace_hours"`
	Rejections        []string  `json:"rejections"`
}

type SwapSubtitle struct {
	Index    int    `json:"index"`
	Language string `json:"language,omitempty"`
	Codec    string `json:"codec"`
	Title    string `json:"title,omitempty"`
}

type SwapProbe struct {
	Subtitles []SwapSubtitle `json:"subtitles"`
	Wanted    SwapSubtitle   `json:"wanted"`
}

type SwapJellyfin struct {
	SeriesID string `json:"series_id"`
	ItemID   string `json:"item_id"`
	Path     string `json:"path"`
}

type SwapBlocklist struct {
	Skip        bool      `json:"skip"`
	Note        string    `json:"note,omitempty"`
	HistoryID   int       `json:"history_id,omitempty"`
	DownloadID  string    `json:"download_id,omitempty"`
	SourceTitle string    `json:"source_title,omitempty"`
	GrabbedAt   time.Time `json:"grabbed_at,omitzero"`
}

type SwapPlan struct {
	What     string `json:"what"`
	Language string `json:"language"`
	Title    string `json:"title"`
	SeriesID int    `json:"series_id"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`

	EpisodeID    int    `json:"episode_id"`
	EpisodeTitle string `json:"episode_title,omitempty"`

	Current   SwapCurrent   `json:"current"`
	Candidate SwapCandidate `json:"candidate"`

	Mapping      []CandidateEpisode `json:"mapping"`
	GuardVerdict string             `json:"guard_verdict"`
	Probe        SwapProbe          `json:"probe"`
	Jellyfin     SwapJellyfin       `json:"jellyfin"`
	Blocklist    SwapBlocklist      `json:"blocklist"`

	RecycleBin       string `json:"recycle_bin"`
	AutoRedownloadOn bool   `json:"auto_redownload"`

	Steps []operation.Step `json:"steps"`
}
