package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
)

const (
	phraseSubtitles        = "looking for subtitles"
	maxBazarrCandidate     = 5
	defaultBazarrPollEvery = 5 * time.Second
	defaultBazarrPollFor   = 90 * time.Second
	paramTarget            = "target"
)

var ErrUnknownLanguage = errors.New("bazarr_search_subtitles: unknown subtitle language")

var errAttemptsUnavailable = errors.New("attempt history is unavailable")

type BazarrSearchResult struct {
	Found      bool                     `json:"found"`
	Language   string                   `json:"language"`
	Summary    string                   `json:"summary"`
	Path       string                   `json:"path,omitempty"`
	Candidates []client.BazarrCandidate `json:"candidates,omitempty"`
	Waited     string                   `json:"waited"`
	Target     string                   `json:"target,omitempty"`
	Movie      bool                     `json:"movie,omitempty"`
	Title      string                   `json:"title,omitempty"`
	Season     int                      `json:"season,omitempty"`
	Episode    int                      `json:"episode,omitempty"`
}

func (s *BazarrSearchResult) coversEpisode(info *client.ItemInfo) bool {
	return s != nil && !s.Movie && strings.EqualFold(s.Title, info.SeriesName) &&
		s.Season == info.Season && s.Episode == info.Episode
}

func (s *BazarrSearchResult) coversMovie(info *client.ItemInfo) bool {
	return s != nil && s.Movie && strings.EqualFold(s.Title, info.Name)
}

func bazarrToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolBazarrSearch,
			Def: &openai.FunctionDefinition{
				Name: toolBazarrSearch,
				Description: "Force Bazarr to search its providers for a missing or broken subtitle language, then " +
					"wait up to 90 s for the outcome. found=true means a new subtitle file was written next to the " +
					"video. found=false lists up to 5 manual candidates for the owner. Autonomous: it only adds a " +
					"file and never deletes or replaces anything. Give season and episode for tv.",
				Parameters: jsonSchema(map[string]any{
					paramMediaType: param("string", "tv|movie"),
					paramTitle:     param("string", "Series or movie title"),
					paramSeason:    param("integer", "Season number (tv only)"),
					paramEpisode:   param("integer", "Episode number (tv only)"),
					paramLanguage:  param("string", "Wanted subtitle language (en, eng or english)"),
				}, []string{paramMediaType, paramTitle, paramLanguage}),
			},
			Risk:       riskWrite,
			Impact:     impactItem,
			Handler:    (*Dispatcher).dispatchBazarrSearch,
			UserPhrase: phraseSubtitles,
		},
	}
}

func (d *Dispatcher) bazarrTarget(
	ctx context.Context, args map[string]any, res *BazarrSearchResult,
) (client.BazarrTarget, string, error) {
	title, _ := args[paramTitle].(string)
	if mediaType, _ := args[paramMediaType].(string); mediaType == client.ReplaceMediaMovie {
		if !d.Radarr.Configured() {
			return client.BazarrTarget{}, "", refuse(errRadarrNotConfigured.Error())
		}
		movie, err := d.Radarr.SearchMovie(ctx, title)
		if err != nil {
			return client.BazarrTarget{}, "", err
		}
		res.Movie, res.Title = true, movie.Title
		return client.BazarrTarget{Movie: true, MovieID: movie.ID}, "movie:" + strconv.Itoa(movie.ID), nil
	}
	if !d.Sonarr.Configured() {
		return client.BazarrTarget{}, "", refuse(errSonarrNotConfigured.Error())
	}
	season, episode := intArgOrSentinel(args, paramSeason), intArgOrSentinel(args, paramEpisode)
	if season < 0 || episode < 0 {
		return client.BazarrTarget{}, "", refuse("season and episode are required for tv")
	}
	series, err := d.Sonarr.SearchSeries(ctx, title)
	if err != nil {
		return client.BazarrTarget{}, "", err
	}
	epID, err := d.findSonarrEpisodeID(ctx, series.ID, season, episode)
	if err != nil {
		return client.BazarrTarget{}, "", err
	}
	res.Title, res.Season, res.Episode = series.Title, season, episode
	return client.BazarrTarget{SeriesID: series.ID, EpisodeID: epID}, "ep:" + strconv.Itoa(epID), nil
}

func externalSubs(subs []client.BazarrSubtitle, iso1 string) map[string]int64 {
	out := map[string]int64{}
	for _, s := range subs {
		if s.Path != "" && s.Code2 == iso1 {
			out[s.Path] = s.FileSize
		}
	}
	return out
}

func newExternalSub(subs []client.BazarrSubtitle, iso1 string, before map[string]int64) string {
	for p, size := range externalSubs(subs, iso1) {
		if old, ok := before[p]; !ok || old != size {
			return p
		}
	}
	return ""
}

func (d *Dispatcher) dispatchBazarrSearch(ctx context.Context, c *toolCall, args map[string]any) (any, error) {
	language, _ := args[paramLanguage].(string)
	iso1, _, known := langCodes(language)
	if !known {
		return nil, refuseCause(ErrUnknownLanguage)
	}
	res := &BazarrSearchResult{Language: iso1}
	target, key, err := d.bazarrTarget(ctx, args, res)
	if err != nil {
		return nil, err
	}
	res.Target = key
	if c.scope.Desync {
		if again, _ := d.searchedElsewhere(ctx, c.scope.IncidentID, res); again {
			return nil, refuse("already searched for this subtitle on another report in the last 24 hours")
		}
	}
	snapshot, err := d.Bazarr.Subtitles(ctx, target)
	if err != nil {
		return nil, err
	}
	before := externalSubs(snapshot, iso1)
	ch, release := d.BazarrWaits.Wait(key)
	defer func() { release() }()
	title, _ := args[paramTitle].(string)
	sctx, actErr := c.act(ctx, map[string]any{
		paramMediaType: args[paramMediaType], paramTitle: title, paramSeason: args[paramSeason],
		paramEpisode: args[paramEpisode], paramLanguage: language, paramTarget: key,
	})
	if actErr != nil {
		return nil, actErr
	}
	if err = d.Bazarr.Search(sctx, target, iso1, false, false); err != nil {
		return nil, err
	}
	start := time.Now()
	every, total := c.poll()
	deadline := time.NewTimer(total)
	defer deadline.Stop()
	tick := time.NewTicker(every)
	defer tick.Stop()
	var readErr error
poll:
	for {
		subs, subsErr := d.Bazarr.Subtitles(ctx, target)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		readErr = subsErr
		if p := newExternalSub(subs, iso1, before); subsErr == nil && p != "" {
			res.Found, res.Path = true, p
			res.Summary = fmt.Sprintf("Downloaded %s subtitles: %s", iso1, path.Base(p))
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			break poll
		case <-tick.C:
		case <-ch:
			release()
			ch, release = d.BazarrWaits.Wait(key)
		}
	}
	res.Waited = time.Since(start).Round(time.Millisecond).String()
	if !res.Found {
		d.fillCandidates(ctx, target, iso1, readErr, res)
	}
	return res, nil
}

func (d *Dispatcher) fillCandidates(
	ctx context.Context, target client.BazarrTarget, iso1 string, readErr error, res *BazarrSearchResult,
) {
	all, err := d.Bazarr.Candidates(ctx, target)
	for _, cand := range all {
		if code, _, ok := langCodes(cand.Language); ok && code == iso1 {
			res.Candidates = append(res.Candidates, cand)
		}
	}
	slices.SortStableFunc(res.Candidates, func(a, b client.BazarrCandidate) int {
		return -cmpFloat(a.Score, b.Score)
	})
	res.Candidates = res.Candidates[:min(len(res.Candidates), maxBazarrCandidate)]
	res.Summary = fmt.Sprintf(
		"No %s subtitles found automatically; %d candidates on the card",
		iso1,
		len(res.Candidates),
	)
	if err != nil {
		res.Summary = fmt.Sprintf("No %s subtitles found automatically; candidates unavailable", iso1)
	}
	if readErr != nil {
		res.Summary += "; the last read of Bazarr's state failed"
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func lastBazarrSearch(messages []openai.ChatCompletionMessage) *BazarrSearchResult {
	calls := map[string]bool{}
	var last *BazarrSearchResult
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = calls[tc.ID] || tc.Function.Name == toolBazarrSearch
		}
		if m.Role != openai.ChatMessageRoleTool || !calls[m.ToolCallID] {
			continue
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(m.Content), &probe) != nil || probe[keyError] != nil {
			continue
		}
		var res BazarrSearchResult
		if json.Unmarshal([]byte(m.Content), &res) == nil {
			last = &res
		}
	}
	return last
}

func (c *toolCall) poll() (time.Duration, time.Duration) {
	every, total := c.pollEvery, c.pollFor
	if every == 0 {
		every = defaultBazarrPollEvery
	}
	if total == 0 {
		total = defaultBazarrPollFor
	}
	return every, total
}
