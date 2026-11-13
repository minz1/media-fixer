package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

const (
	toolSonarrSwapPlan = "sonarr_swap_plan"
	paramDownloadID    = "download_id"
	appSonarr          = "sonarr"
	appJellyfin        = "jellyfin"
	swapGraceHours     = 168
	swapGuardOK        = "ok"
)

const swapDownloadsRoot = "/data/downloads/sonarr"

func (d *Dispatcher) swapFolder(ctx context.Context, downloadID string) (string, error) {
	e, err := d.Decypharr.EntryByID(ctx, downloadID)
	if errors.Is(err, client.ErrNoDecypharrEntry) {
		return "", fmt.Errorf("download id %s is not cached in decypharr", downloadID)
	}
	if err != nil {
		return "", err
	}
	if e.ContentPath == "" {
		return "", fmt.Errorf("decypharr has no download folder for download id %s", downloadID)
	}
	folder := filepath.Clean(e.ContentPath)
	base := filepath.Base(folder)
	if filepath.Dir(folder) != swapDownloadsRoot || base == ".." || strings.ContainsRune(base, 0) {
		return "", fmt.Errorf(
			"decypharr's folder %q for download id %s is not directly under %s", e.ContentPath, downloadID,
			swapDownloadsRoot,
		)
	}
	return folder, nil
}

func swapSpecs() []toolSpec {
	return []toolSpec{{
		Name: toolSonarrSwapPlan,
		Def: &openai.FunctionDefinition{
			Name: toolSonarrSwapPlan,
			Description: "Read-only plan to swap an episode's broken file for a release Sonarr already grabbed " +
				"and decypharr still has cached (a download_id from list_alternatives). Checks " +
				"the candidate maps to exactly this episode, has the wanted subtitle language with no " +
				"unknown-codec track, and agrees with Jellyfin. Returns the plan the owner approves; it changes " +
				"nothing.",
			Parameters: jsonSchema(map[string]any{
				paramTitle:      param("string", "Series title"),
				paramSeason:     param("integer", "Season number"),
				paramEpisode:    param("integer", "Episode number"),
				paramDownloadID: param("string", "The release's download_id, as listed in list_alternatives"),
				paramLanguage:   param("string", "Wanted subtitle language (en, eng or english)"),
			}, []string{paramTitle, paramSeason, paramEpisode, paramDownloadID, paramLanguage}),
		},
		Risk:    riskRead,
		Handler: (*Dispatcher).readSwapPlan,
	}}
}

func qualityName(raw json.RawMessage) string {
	var q struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	}
	_ = json.Unmarshal(raw, &q)
	return q.Quality.Name
}

func episodeCodes(eps []client.CandidateEpisode) string {
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = fmt.Sprintf("S%02dE%02d", e.SeasonNumber, e.EpisodeNumber)
	}
	return strings.Join(out, ", ")
}

type swapTarget struct {
	series  *client.Series
	episode client.Episode
	file    client.EpisodeFile
}

func (d *Dispatcher) swapTarget(
	ctx context.Context,
	title string,
	season, episode int,
) (*swapTarget, []client.Episode, error) {
	series, err := d.Sonarr.SearchSeries(ctx, title)
	if err != nil {
		return nil, nil, err
	}
	eps, err := d.Sonarr.GetEpisodes(ctx, series.ID, season)
	if err != nil {
		return nil, nil, err
	}
	t := &swapTarget{series: series}
	for _, e := range eps {
		if e.EpisodeNumber == episode {
			t.episode = e
		}
	}
	if t.episode.ID == 0 || !t.episode.HasFile || t.episode.EpisodeFileID == 0 {
		return nil, nil, errors.New("nothing to swap: the episode has no file in Sonarr")
	}
	files, err := d.Sonarr.GetEpisodeFiles(ctx, series.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		if f.ID == t.episode.EpisodeFileID {
			t.file = f
		}
	}
	if t.file.ID == 0 {
		return nil, nil, errors.New("nothing to swap: Sonarr has no record of the episode's file")
	}
	return t, eps, nil
}

func (d *Dispatcher) readSwapPlan(ctx context.Context, c *toolCall, args map[string]any) (any, error) {
	if c.scope.What == WhatStuckRead {
		return nil, errors.New(
			"sonarr_swap_plan is disabled for stuck_read incidents: a stuck read has no subtitle to swap for",
		)
	}
	title, _ := args[paramTitle].(string)
	downloadID, _ := args[paramDownloadID].(string)
	language, _ := args[paramLanguage].(string)
	season, err := intArg(args, paramSeason)
	if err != nil {
		return nil, err
	}
	episode, err := intArg(args, paramEpisode)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(downloadID) == "" {
		return nil, fmt.Errorf("%s is required", paramDownloadID)
	}
	_, want, ok := langCodes(language)
	if !ok {
		return nil, fmt.Errorf("a known %s is required, got %q", paramLanguage, language)
	}
	if season < 0 || episode < 0 {
		return nil, fmt.Errorf("%s and %s are required", paramSeason, paramEpisode)
	}
	tgt, eps, err := d.swapTarget(ctx, title, season, episode)
	if err != nil {
		return nil, err
	}
	plan := &client.SwapPlan{
		What: WhatSubtitles, Language: want, Title: tgt.series.Title, SeriesID: tgt.series.ID,
		Season: season, Episode: episode, EpisodeID: tgt.episode.ID, EpisodeTitle: tgt.episode.Title,
	}
	folder, err := d.swapFolder(ctx, downloadID)
	if err != nil {
		return nil, err
	}
	if err = d.swapCandidate(ctx, plan, tgt, eps, folder, downloadID); err != nil {
		return nil, err
	}
	if err = d.swapProbe(ctx, plan, want); err != nil {
		return nil, err
	}
	if err = d.swapJellyfin(ctx, plan, tgt); err != nil {
		return nil, err
	}
	if plan.RecycleBin, err = d.Sonarr.RecycleBin(ctx); err != nil {
		return nil, err
	}
	cfg, err := d.Sonarr.DownloadClientConfig(ctx)
	if err != nil {
		return nil, err
	}
	plan.AutoRedownloadOn = cfg.AutoRedownloadFailed
	plan.Steps = swapSteps(plan, tgt)
	return plan, nil
}

func (d *Dispatcher) swapCandidate(
	ctx context.Context, plan *client.SwapPlan, tgt *swapTarget, eps []client.Episode, folder, downloadID string,
) error {
	grab, err := d.Sonarr.GrabBehindFile(ctx, tgt.series.ID, tgt.episode.ID, tgt.file.Path)
	if err != nil {
		return err
	}
	cands, err := d.Sonarr.ManualImportFolderCandidates(ctx, folder)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return fmt.Errorf("not cached under %s", folder)
	}
	var mapped []client.ImportCandidate
	for _, cand := range cands {
		for _, e := range cand.Episodes {
			if e.ID == tgt.episode.ID {
				mapped = append(mapped, cand)
				break
			}
		}
	}
	if len(mapped) == 0 && len(cands) == 1 {
		mapped = cands
	}
	if len(mapped) != 1 {
		return fmt.Errorf("%d files under %s map to this episode, need exactly one", len(mapped), folder)
	}
	cand := mapped[0]
	queue, err := d.Sonarr.QueueEntries(ctx)
	if err != nil {
		return err
	}
	if err = stuckqueue.SwapGuard(
		tgt.series.ID, tgt.episode.ID, plan.Season, tgt.file.ID, tgt.file.Path, swapDownloadsRoot, cand, eps, queue,
	); err != nil {
		return fmt.Errorf("%w (Sonarr parsed %s as %s)", err, path.Base(cand.Path), episodeCodes(cand.Episodes))
	}
	events, err := d.Sonarr.SeriesHistory(ctx, tgt.series.ID)
	if err != nil {
		return err
	}
	candGrab := candidateGrab(events, tgt.episode.ID, downloadID)
	if candGrab.DownloadID == "" {
		return fmt.Errorf("no Sonarr grab of this episode has download id %s", downloadID)
	}
	downloadID = candGrab.DownloadID
	grab = grab.WithCandidate(downloadID)
	plan.Current = client.SwapCurrent{
		FileID: tgt.file.ID, SourceTitle: grab.SourceTitle, GrabbedAt: grab.GrabbedAt, DownloadID: grab.DownloadID,
		PackSize: len(grab.EpisodeIDs),
	}
	plan.Current.SwapFile = client.SwapFile{
		Path: tgt.file.Path, Size: tgt.file.Size, ReleaseGroup: tgt.file.ReleaseGroup,
		Quality: tgt.file.Quality.Quality.Name,
	}
	plan.Candidate = client.SwapCandidate{
		Folder: folder, DownloadID: downloadID, CustomFormatScore: cand.CustomFormatScore,
		GrabbedAt: candGrab.Date, GraceHours: swapGraceHours, Rejections: cand.Rejections,
	}
	plan.Candidate.SwapFile = client.SwapFile{
		Path: cand.Path, Size: cand.Size, ReleaseGroup: cand.ReleaseGroup, Quality: qualityName(cand.Quality),
	}
	if plan.Candidate.Rejections == nil {
		plan.Candidate.Rejections = []string{}
	}
	plan.Candidate.GrabAgeHours = int(time.Since(candGrab.Date).Hours())
	plan.Mapping, plan.GuardVerdict = cand.Episodes, swapGuardOK
	plan.Blocklist = client.SwapBlocklist{
		Skip: grab.SkipBlocklist, Note: grab.Note, HistoryID: grab.HistoryID, DownloadID: grab.DownloadID,
		SourceTitle: grab.SourceTitle, GrabbedAt: grab.GrabbedAt,
	}
	return nil
}

func candidateGrab(events []client.SeriesEvent, episodeID int, downloadID string) client.SeriesEvent {
	var best client.SeriesEvent
	for _, e := range events {
		if e.EventType != "grabbed" || e.EpisodeID != episodeID {
			continue
		}
		if strings.EqualFold(e.DownloadID, downloadID) && e.Date.After(best.Date) {
			best = e
		}
	}
	return best
}

func (d *Dispatcher) swapProbe(ctx context.Context, plan *client.SwapPlan, want string) error {
	res, err := d.MediaAgent.ProbeStreams(ctx, plan.Candidate.Path)
	if err != nil {
		return err
	}
	if res.Error != "" || res.NotFound {
		return fmt.Errorf("probe of %s failed: %s", plan.Candidate.Path, res.Error)
	}
	plan.Probe.Subtitles = []client.SwapSubtitle{}
	found := false
	for _, s := range res.Streams {
		if s.CodecType != codecSubtitle {
			continue
		}
		sub := client.SwapSubtitle{Index: s.Index, Language: s.Language, Codec: s.CodecName, Title: s.Title}
		if s.CodecName == "" {
			return fmt.Errorf("the candidate has an unknown-codec subtitle track (stream %d)", s.Index)
		}
		plan.Probe.Subtitles = append(plan.Probe.Subtitles, sub)
		if _, iso3, known := langCodes(s.Language); !found && known && iso3 == want {
			plan.Probe.Wanted, found = sub, true
		}
	}
	if !found {
		return fmt.Errorf("the candidate has no %s subtitle track", want)
	}
	return nil
}

func (d *Dispatcher) swapJellyfin(ctx context.Context, plan *client.SwapPlan, tgt *swapTarget) error {
	items, err := d.Jellyfin.SearchItem(ctx, tgt.series.Title)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Type == itemSeries {
			plan.Jellyfin.SeriesID = it.ID
			break
		}
	}
	if plan.Jellyfin.SeriesID == "" {
		return fmt.Errorf("jellyfin has no series %q", tgt.series.Title)
	}
	eps, err := d.Jellyfin.ListEpisodes(ctx, plan.Jellyfin.SeriesID)
	if err != nil {
		return err
	}
	for _, e := range eps {
		if e.IndexNumber != nil && e.ParentIndexNumber != nil && *e.IndexNumber == plan.Episode &&
			*e.ParentIndexNumber == plan.Season {
			plan.Jellyfin.ItemID, plan.Jellyfin.Path = e.ID, e.Path
		}
	}
	if plan.Jellyfin.ItemID == "" {
		return errors.New("jellyfin has no item for this episode")
	}
	if path.Base(plan.Jellyfin.Path) != path.Base(tgt.file.Path) {
		return fmt.Errorf(
			"jellyfin and sonarr disagree on the file: %s vs %s",
			path.Base(plan.Jellyfin.Path),
			path.Base(tgt.file.Path),
		)
	}
	return nil
}

func swapSteps(plan *client.SwapPlan, tgt *swapTarget) []operation.Step {
	steps := []operation.Step{
		{
			Kind: operation.StepImport, App: appSonarr, Path: plan.Candidate.Path, Size: plan.Candidate.Size,
			FileID: tgt.file.ID, MediaID: plan.SeriesID, EpisodeIDs: []int{plan.EpisodeID}, Season: plan.Season,
			Episode: plan.Episode,
		},
		{
			Kind:    operation.StepJellyfinNotify,
			App:     appJellyfin,
			Path:    plan.Jellyfin.Path,
			Name:    plan.Jellyfin.SeriesID,
			Season:  plan.Season,
			Episode: plan.Episode,
		},
	}
	if !plan.Blocklist.Skip {
		steps = append(steps, operation.Step{
			Kind:       operation.StepBlocklist,
			App:        appSonarr,
			HistoryID:  plan.Blocklist.HistoryID,
			DownloadID: plan.Blocklist.DownloadID,
			GrabbedAt:  plan.Blocklist.GrabbedAt,
			Name:       plan.Blocklist.SourceTitle,
			MediaID:    plan.SeriesID,
			Season:     plan.Season,
			Episode:    plan.Episode,
		})
	}
	return steps
}

func (d *Dispatcher) planSwap(ctx context.Context, result *DiagnosticResult, what string) (any, error) {
	if what != WhatSubtitles {
		return nil, fmt.Errorf("%s only applies to %s incidents, not %q", EscalateSwapRelease, WhatSubtitles, what)
	}
	out, err := d.readSwapPlan(ctx, &toolCall{scope: Scope{What: what}}, result.EscalateParams)
	if err != nil {
		return nil, err
	}
	if plan, ok := out.(*client.SwapPlan); ok {
		if err = swapBinding(result, plan); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func swapBinding(result *DiagnosticResult, plan *client.SwapPlan) error {
	want := result.SubtitleItemID
	if want == "" {
		want = result.IncidentItemID
	}
	if want == "" {
		return errors.New("the incident's episode is unknown")
	}
	if plan.Jellyfin.ItemID != want {
		return fmt.Errorf(
			"the planned episode (jellyfin item %s) does not match the incident's episode (%s)",
			plan.Jellyfin.ItemID,
			want,
		)
	}
	if _, iso3, ok := langCodes(result.VerifySubtitleLanguage); !ok || iso3 != plan.Language {
		return fmt.Errorf(
			"the plan's language %s does not match the finding's verify_subtitle_language %q",
			plan.Language, result.VerifySubtitleLanguage,
		)
	}
	return nil
}

func (d *Dispatcher) swapOperation(
	subject operation.Subject,
	result *DiagnosticResult,
	planJSON []byte,
) (operation.Spec, error) {
	var plan client.SwapPlan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return operation.Spec{}, fmt.Errorf("decode approved swap plan: %w", err)
	}
	if plan.What != WhatSubtitles || plan.Language == "" {
		return operation.Spec{}, fmt.Errorf(
			"%s only applies to %s incidents with a wanted language", EscalateSwapRelease, WhatSubtitles,
		)
	}
	if err := swapBinding(result, &plan); err != nil {
		return operation.Spec{}, err
	}
	return opexec.SwapSpec(subject, &plan, planJSON)
}
