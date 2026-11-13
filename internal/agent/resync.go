package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
)

const (
	toolBazarrResync       = "bazarr_resync_subtitle"
	phraseResync           = "resyncing subtitles"
	defaultResyncPollEvery = 15 * time.Second
	defaultResyncPollFor   = 20 * time.Minute
	resyncGuardWindow      = 24 * time.Hour
	paramPath              = "path"
	bytesPerGB             = 1 << 30
)

var resyncOffsetRE = regexp.MustCompile(`offset of (-?[0-9.]+) seconds`)

type BazarrResyncResult struct {
	Synced   bool   `json:"synced"`
	Summary  string `json:"summary"`
	Language string `json:"language"`
	Path     string `json:"path,omitempty"`
	Offset   string `json:"offset,omitempty"`
	Waited   string `json:"waited"`
	Movie    bool   `json:"movie,omitempty"`
	Title    string `json:"title,omitempty"`
	Season   int    `json:"season,omitempty"`
	Episode  int    `json:"episode,omitempty"`
}

func (r *BazarrResyncResult) coversEpisode(info *client.ItemInfo) bool {
	return r != nil && !r.Movie && strings.EqualFold(r.Title, info.SeriesName) &&
		r.Season == info.Season && r.Episode == info.Episode
}

func (r *BazarrResyncResult) coversMovie(info *client.ItemInfo) bool {
	return r != nil && r.Movie && strings.EqualFold(r.Title, info.Name)
}

func resyncToolSpec() toolSpec {
	return toolSpec{
		Name: toolBazarrResync,
		Def: &openai.FunctionDefinition{
			Name: toolBazarrResync,
			Description: "Only for a subtitles-out-of-sync report, after bazarr_search_subtitles did not fix it: ask " +
				"Bazarr to align the one external subtitle file of this language to the audio, then wait up to 20 " +
				"minutes for Bazarr to report the result. Rewrites that subtitle file in place. synced=true means " +
				"Bazarr finished the sync. Give season and episode for tv.",
			Parameters: jsonSchema(map[string]any{
				paramMediaType: param("string", "tv|movie"),
				paramTitle:     param("string", "Series or movie title"),
				paramSeason:    param("integer", "Season number (tv only)"),
				paramEpisode:   param("integer", "Episode number (tv only)"),
				paramLanguage:  param("string", "Subtitle language to resync (en, eng or english)"),
			}, []string{paramMediaType, paramTitle, paramLanguage}),
		},
		Risk:       riskWrite,
		Impact:     impactItem,
		Handler:    (*Dispatcher).dispatchBazarrResync,
		UserPhrase: phraseResync,
	}
}

func resyncOffset(description string) string {
	m := resyncOffsetRE.FindStringSubmatch(description)
	if m == nil {
		return ""
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%+.1f s", v)
}

func (c *toolCall) resyncPoll() (time.Duration, time.Duration) {
	every, total := c.pollEvery, c.pollFor
	if every == 0 {
		every = defaultResyncPollEvery
	}
	if total == 0 {
		total = defaultResyncPollFor
	}
	return every, total
}

func (d *Dispatcher) videoSize(ctx context.Context, target client.BazarrTarget, season int) (int64, error) {
	if target.Movie {
		files, err := d.Radarr.GetMovieFiles(ctx, target.MovieID)
		if err != nil {
			return 0, refuse("could not read the movie file size: " + err.Error())
		}
		if len(files) != 1 {
			return 0, refuse(fmt.Sprintf("radarr lists %d files for this movie, want exactly one", len(files)))
		}
		return files[0].Size, nil
	}
	episodes, err := d.Sonarr.GetEpisodes(ctx, target.SeriesID, season)
	if err != nil {
		return 0, refuse("could not read the episode file: " + err.Error())
	}
	fileID := 0
	for _, ep := range episodes {
		if ep.ID == target.EpisodeID {
			fileID = ep.EpisodeFileID
		}
	}
	if fileID == 0 {
		return 0, refuse("sonarr has no file for this episode")
	}
	files, err := d.Sonarr.GetEpisodeFiles(ctx, target.SeriesID)
	if err != nil {
		return 0, refuse("could not read the episode file size: " + err.Error())
	}
	for _, f := range files {
		if f.ID == fileID {
			return f.Size, nil
		}
	}
	return 0, refuse("sonarr does not list this episode's file")
}

func resyncSidecar(subs []client.BazarrSubtitle, iso1 string) (client.BazarrSubtitle, error) {
	var plain, hi []client.BazarrSubtitle
	for _, s := range subs {
		switch strings.ToLower(path.Ext(s.Path)) {
		case ".srt", ".ass", ".ssa":
		default:
			continue
		}
		if s.Code2 != iso1 || s.Forced {
			continue
		}
		if s.HI {
			hi = append(hi, s)
		} else {
			plain = append(plain, s)
		}
	}
	pick := plain
	if len(pick) == 0 {
		pick = hi
	}
	if len(pick) != 1 {
		return client.BazarrSubtitle{}, refuse(fmt.Sprintf(
			"want exactly one external %s .srt, .ass or .ssa subtitle that is not forced, found %d", iso1, len(pick)))
	}
	return pick[0], nil
}

func (d *Dispatcher) resyncedRecently(ctx context.Context, subPath string) error {
	if d.Attempts == nil {
		return refuse("resync attempt history is unavailable")
	}
	attempts, err := d.Attempts.AttemptsSince(ctx, toolBazarrResync, time.Now().Add(-resyncGuardWindow))
	if err != nil {
		return refuse("could not read earlier resync attempts: " + err.Error())
	}
	for _, a := range attempts {
		if p, _ := a.Params[paramPath].(string); p == subPath {
			return refuse("this subtitle was already resynced in the last 24 hours")
		}
	}
	return nil
}

func (d *Dispatcher) dispatchBazarrResync(ctx context.Context, c *toolCall, args map[string]any) (any, error) {
	language, _ := args[paramLanguage].(string)
	iso1, _, known := langCodes(language)
	if !known {
		return nil, refuseCause(ErrUnknownLanguage)
	}
	if c.scope.What != WhatSubtitles {
		return nil, refuse("resync only runs on a subtitles report")
	}
	scratch := &BazarrSearchResult{}
	target, key, err := d.bazarrTarget(ctx, args, scratch)
	if err != nil {
		return nil, err
	}
	res := &BazarrResyncResult{
		Language: iso1, Movie: scratch.Movie, Title: scratch.Title, Season: scratch.Season, Episode: scratch.Episode,
	}
	size, err := d.videoSize(ctx, target, scratch.Season)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, refuse("the video file size is unknown")
	}
	if size > d.ResyncMaxBytes {
		return nil, refuse(fmt.Sprintf(
			"the video is %d GB, over the %d GB resync cap", size/bytesPerGB, d.ResyncMaxBytes/bytesPerGB))
	}
	subs, err := d.Bazarr.Subtitles(ctx, target)
	if err != nil {
		return nil, err
	}
	sub, err := resyncSidecar(subs, iso1)
	if err != nil {
		return nil, err
	}
	if err = d.resyncedRecently(ctx, sub.Path); err != nil {
		return nil, err
	}
	baseline, _, err := d.Bazarr.SyncHistory(ctx, target, sub.Path)
	if err != nil {
		return nil, err
	}
	every, total := c.resyncPoll()
	if c.gate != nil {
		c.gate.HoldStale(int((total + time.Minute - 1) / time.Minute))
	}
	title, _ := args[paramTitle].(string)
	sctx, err := c.act(ctx, map[string]any{
		paramMediaType: args[paramMediaType], paramTitle: title, paramSeason: args[paramSeason],
		paramEpisode: args[paramEpisode], paramLanguage: language, paramPath: sub.Path,
	})
	if err != nil {
		return nil, err
	}
	if err = d.Bazarr.Sync(sctx, target, sub.Path, iso1, sub.HI); err != nil {
		return nil, err
	}
	res.Path = sub.Path
	start := time.Now()
	if err = d.awaitSync(ctx, key, target, baseline, every, total, res); err != nil {
		return nil, err
	}
	res.Waited = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

func (d *Dispatcher) awaitSync(
	ctx context.Context, key string, target client.BazarrTarget, baseline int, every, total time.Duration,
	res *BazarrResyncResult,
) error {
	ch, release := d.BazarrWaits.Wait(key)
	defer func() { release() }()
	deadline := time.NewTimer(total)
	defer deadline.Stop()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		count, latest, readErr := d.Bazarr.SyncHistory(ctx, target, res.Path)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if readErr == nil && count > baseline {
			res.Synced, res.Offset = true, resyncOffset(latest)
			res.Summary = resyncSummary(res, total, nil)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			res.Summary = resyncSummary(res, total, readErr)
			return nil
		case <-tick.C:
		case <-ch:
			release()
			ch, release = d.BazarrWaits.Wait(key)
		}
	}
}

func resyncSummary(res *BazarrResyncResult, total time.Duration, readErr error) string {
	if res.Synced {
		if res.Offset == "" {
			return fmt.Sprintf("Resynced %s subtitles to the audio", res.Language)
		}
		return fmt.Sprintf("Resynced %s subtitles to the audio (offset %s)", res.Language, res.Offset)
	}
	wait := total.String()
	if total >= time.Minute {
		wait = fmt.Sprintf("%d minutes", int(total/time.Minute))
	}
	s := fmt.Sprintf("Bazarr did not report a finished sync of %s in %s (a forced subtitle or a low-quality "+
		"alignment is skipped); Bazarr may still finish later and rewrite the file", path.Base(res.Path), wait)
	if readErr != nil {
		s += "; the last read of Bazarr's state failed"
	}
	return s
}

func lastBazarrResync(messages []openai.ChatCompletionMessage) *BazarrResyncResult {
	calls := map[string]bool{}
	var last *BazarrResyncResult
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = calls[tc.ID] || tc.Function.Name == toolBazarrResync
		}
		if m.Role != openai.ChatMessageRoleTool || !calls[m.ToolCallID] {
			continue
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(m.Content), &probe) != nil || probe[keyError] != nil {
			continue
		}
		var res BazarrResyncResult
		if json.Unmarshal([]byte(m.Content), &res) == nil {
			last = &res
		}
	}
	return last
}
