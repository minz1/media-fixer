package client

import (
	"context"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
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
