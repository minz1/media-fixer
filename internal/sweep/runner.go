package sweep

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	keepRuns       = 50
	seasonAttempts = 4
	animeAttempts  = 3
	animeBatch     = 10
	grabGap        = 60 * time.Second
	grabPoll       = 30 * time.Second
	grabWait       = 7 * time.Minute
	postCheckDelay = 30 * time.Second
	historySkew    = 5 * time.Minute
	getRetryEvery  = 20 * time.Second
	getRetryFor    = 10 * time.Minute
	refusedMemory  = 7 * 24 * time.Hour
	BlockedMemory  = 90 * 24 * time.Hour
	noPackMemory   = 30 * 24 * time.Hour
	searchGap      = 60 * time.Second
	commandPoll    = 15 * time.Second
	commandWait    = 15 * time.Minute

	seriesAnime = "anime"

	statusDone        = "done"
	statusFailed      = "failed"
	statusPaused      = "paused"
	statusRefused     = "refused"
	statusInterrupted = "interrupted"

	resultGrabbing    = "grabbing"
	resultImported    = "imported"
	resultFailed      = "failed"
	resultRefused     = "refused"
	resultArchive     = "archive"
	resultDownloading = "downloading"
	resultNoPack      = "no_pack"
	resultWouldGrab   = "would_grab"
	resultSearched    = "searched"
	resultWouldSearch = "would_search"
)

var (
	ErrPaused     = errors.New("paused")
	errRedownload = errors.New("autoRedownloadFailedFromInteractiveSearch is on")
	errCapReached = errors.New("cap reached")
)

type Runner struct {
	Sonarr  *client.ArrClient
	DB      *db.DB
	Notify  func(context.Context, card.Card) error
	Refresh func(string)
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
	Log     *slog.Logger

	mu       sync.Mutex
	progress *Progress
}

type Progress struct {
	RunID          int64
	Phase, Current string
	Dry            bool
	Counts         db.SweepCounts
}

func (r *Runner) Progress() (Progress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.progress == nil {
		return Progress{}, false
	}
	return *r.progress, true
}

type run struct {
	*Runner

	id         int64
	dry        bool
	s          Settings
	c          db.SweepCounts
	refused    map[string]bool
	skip       map[string]bool
	queued     map[int]bool
	tried      map[string]bool
	handled    map[int]bool
	imported   []string
	lastGrab   time.Time
	lastSearch time.Time
}

type seasonTarget struct {
	series client.Series
	season int
	eps    []client.Episode
}

type searchBatch struct {
	item  db.SweepItem
	eps   []client.Episode
	anime bool
}

func (r *Runner) Run(ctx context.Context, trigger string, dry bool, s Settings) (int64, error) {
	if err := r.DB.PruneSweepRuns(ctx, keepRuns, r.Now().AddDate(0, 0, -8)); err != nil {
		return 0, err
	}
	start := r.Now()
	id, err := r.DB.CreateSweepRun(ctx, trigger, dry, start)
	if err != nil {
		return 0, err
	}
	x := &run{
		Runner: r, id: id, dry: dry, s: s, tried: map[string]bool{}, handled: map[int]bool{},
		c: db.SweepCounts{MaxGrabs: s.MaxGrabs, MaxLookups: s.MaxLookups, MaxSearches: s.MaxSearches},
	}
	x.report("preflight", "")
	tracked := r.notifyStart(ctx, startCard(id, trigger, dry, s))
	err = x.sweep(ctx)
	r.mu.Lock()
	r.progress = nil
	r.mu.Unlock()

	status, reason := statusDone, ""
	switch {
	case errors.Is(err, ErrPaused):
		status, err = statusPaused, nil
	case errors.Is(err, errRedownload):
		status, reason, err = statusRefused, errRedownload.Error(), nil
	case ctx.Err() != nil:
		status, reason = statusInterrupted, context.Cause(ctx).Error()
	case err != nil:
		status, reason = statusFailed, err.Error()
	}
	fctx := context.WithoutCancel(ctx)
	end := r.Now()
	if ferr := r.DB.FinishSweepRun(fctx, id, status, reason, x.c, end); ferr != nil {
		return id, errors.Join(err, ferr)
	}
	switch {
	case tracked:
		if r.Refresh != nil {
			r.Refresh(card.SweepSubject(id))
		}
	case r.Notify != nil:
		sr := db.SweepRun{
			ID:         id,
			Trigger:    trigger,
			Dry:        dry,
			StartedAt:  start,
			FinishedAt: &end,
			Status:     status,
			Reason:     reason,
			Counts:     x.c,
		}
		if nerr := r.Notify(fctx, summaryCard(sr)); nerr != nil {
			r.logger().WarnContext(fctx, "sweep summary DM failed", "run", id, "err", nerr)
		}
	}
	return id, err
}

func (x *run) sweep(ctx context.Context) error {
	if err := x.guard(ctx); err != nil {
		return err
	}
	now := x.Now()
	rows, err := x.DB.ListSweepRefused(ctx, now.Add(-BlockedMemory))
	if err != nil {
		return err
	}
	x.refused = map[string]bool{}
	for _, r := range Blocking(rows, now) {
		x.refused[r.TitleKey] = true
	}
	if x.skip, err = x.DB.NoPackTargets(ctx, now.Add(-noPackMemory)); err != nil {
		return err
	}
	queue, err := retry(ctx, x, false, func() ([]client.QueueDetail, error) { return x.Sonarr.QueueDetails(ctx) })
	if err != nil {
		return err
	}
	x.queued = QueuedEpisodes(queue)
	series, err := retry(ctx, x, false, func() ([]client.Series, error) { return x.Sonarr.ListSeries(ctx) })
	if err != nil {
		return err
	}
	var eligible []client.Series
	for _, s := range series {
		if s.Monitored && !slices.Contains(x.s.ExcludedSeries, s.ID) && (s.SeriesType != seriesAnime || x.s.Anime) {
			eligible = append(eligible, s)
		}
	}
	if err = x.packPasses(ctx, eligible); err != nil && !errors.Is(err, errCapReached) {
		return err
	}
	if err = x.episodePass(ctx, eligible); err != nil {
		return err
	}
	return x.cleanup(ctx)
}

func (x *run) packPasses(ctx context.Context, series []client.Series) error {
	if err := x.seasonPass(ctx, series); err != nil {
		return err
	}
	for _, s := range series {
		if s.SeriesType != seriesAnime {
			continue
		}
		if err := x.animeSeries(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (x *run) missing(ctx context.Context, s client.Series) ([]client.Episode, error) {
	eps, err := retry(ctx, x, false, func() ([]client.Episode, error) {
		return x.Sonarr.GetEpisodes(ctx, s.ID, -1)
	})
	if err != nil {
		return nil, err
	}
	missing := Missing(eps, x.queued, x.Now())
	return slices.DeleteFunc(missing, func(e client.Episode) bool { return x.handled[e.ID] }), nil
}

func (x *run) seasonPass(ctx context.Context, series []client.Series) error {
	x.report("season pass", "")
	var targets []seasonTarget
	for _, s := range series {
		if s.SeriesType == seriesAnime {
			continue
		}
		missing, err := x.missing(ctx, s)
		if err != nil {
			return err
		}
		bySeason := map[int][]client.Episode{}
		for _, e := range missing {
			bySeason[e.SeasonNumber] = append(bySeason[e.SeasonNumber], e)
		}
		for season, missing := range bySeason {
			if len(missing) >= x.s.MinMissing && !x.skip[seasonTargetKey(s.ID, season)] {
				targets = append(targets, seasonTarget{series: s, season: season, eps: missing})
			}
		}
	}
	slices.SortFunc(targets, func(a, b seasonTarget) int {
		return cmp.Or(len(b.eps)-len(a.eps), a.series.ID-b.series.ID, a.season-b.season)
	})
	for _, t := range targets {
		if err := x.season(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

func (x *run) season(ctx context.Context, t seasonTarget) error {
	season := t.season
	item := db.SweepItem{
		RunID: x.id, Target: seasonTargetKey(t.series.ID, t.season), SeriesID: t.series.ID,
		SeriesTitle: t.series.Title, Season: &season, Episodes: numbers(t.eps, false),
	}
	rels, ok, err := x.lookup(ctx, &item, "season pass", fmt.Sprintf("%s S%02d", t.series.Title, t.season),
		func() ([]client.Release, error) { return x.Sonarr.Releases(ctx, t.series.ID, t.season) })
	if !ok {
		return err
	}
	cands := SeasonCandidates(rels, x.refused)
	Rank(cands)
	_, handled, err := x.attempts(ctx, item, cands, seasonAttempts)
	if handled {
		for _, e := range t.eps {
			x.handled[e.ID] = true
		}
	}
	return err
}

func (x *run) animeSeries(ctx context.Context, s client.Series) error {
	missing, err := x.missing(ctx, s)
	if err != nil {
		return err
	}
	slices.SortFunc(missing, byAbsolute)
	uncovered := map[int]bool{}
	for _, e := range missing {
		uncovered[e.ID] = true
	}
	for _, e := range missing {
		if !uncovered[e.ID] || x.skip[episodeTargetKey(e.ID)] {
			continue
		}
		item := db.SweepItem{
			RunID: x.id, Target: episodeTargetKey(e.ID), SeriesID: s.ID, SeriesTitle: s.Title,
			Episodes: strconv.Itoa(e.AbsoluteEpisodeNumber),
		}
		rels, ok, lerr := x.lookup(ctx, &item, "anime pass", s.Title+" "+item.Episodes,
			func() ([]client.Release, error) { return x.Sonarr.EpisodeReleases(ctx, e.ID) })
		if !ok {
			if lerr != nil {
				return lerr
			}
			continue
		}
		cands := AnimeCandidates(rels, uncovered, x.refused)
		Rank(cands)
		rel, handled, aerr := x.attempts(ctx, item, cands, animeAttempts)
		if handled {
			for _, id := range Covers(rel, uncovered) {
				delete(uncovered, id)
				x.handled[id] = true
			}
		}
		if aerr != nil {
			return aerr
		}
	}
	return nil
}

func (x *run) lookup(
	ctx context.Context,
	item *db.SweepItem,
	phase, current string,
	f func() ([]client.Release, error),
) ([]client.Release, bool, error) {
	if x.c.Lookups >= x.s.MaxLookups || x.c.Grabs >= x.s.MaxGrabs {
		return nil, false, errCapReached
	}
	if err := x.guard(ctx); err != nil {
		return nil, false, err
	}
	x.c.Lookups++
	x.report(phase, current)
	began := x.Now()
	rels, err := retry(ctx, x, true, f)
	n, ms := len(rels), int(x.Now().Sub(began).Milliseconds())
	item.LookupResults, item.LookupMS = &n, &ms
	if err == nil {
		return rels, true, nil
	}
	if ctx.Err() != nil {
		return nil, false, err
	}
	x.logger().WarnContext(ctx, "sweep lookup failed", "target", item.Target, "err", err)
	item.Result = resultFailed
	return nil, false, x.addItem(ctx, item)
}

func (x *run) episodePass(ctx context.Context, series []client.Series) error {
	if x.s.MaxSearches == 0 {
		return nil
	}
	x.report("episode pass", "")
	queue, err := retry(ctx, x, false, func() ([]client.QueueDetail, error) { return x.Sonarr.QueueDetails(ctx) })
	if err != nil {
		return err
	}
	x.queued = QueuedEpisodes(queue)
	var batches []searchBatch
	for _, s := range series {
		missing, merr := x.missing(ctx, s)
		if merr != nil {
			return merr
		}
		batches = append(batches, searchBatches(x.id, s, missing)...)
	}
	for _, b := range batches {
		if x.c.Searched >= x.s.MaxSearches {
			return nil
		}
		if err = x.search(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

func searchBatches(runID int64, s client.Series, missing []client.Episode) []searchBatch {
	base := db.SweepItem{RunID: runID, SeriesID: s.ID, SeriesTitle: s.Title}
	var out []searchBatch
	if s.SeriesType == seriesAnime {
		slices.SortFunc(missing, byAbsolute)
		for chunk := range slices.Chunk(missing, animeBatch) {
			b := searchBatch{item: base, eps: chunk, anime: true}
			b.item.Target = episodeTargetKey(chunk[0].ID)
			out = append(out, b)
		}
		return out
	}
	slices.SortFunc(missing, func(a, b client.Episode) int {
		return cmp.Or(a.SeasonNumber-b.SeasonNumber, a.EpisodeNumber-b.EpisodeNumber)
	})
	for i := 0; i < len(missing); {
		j := i
		for j < len(missing) && missing[j].SeasonNumber == missing[i].SeasonNumber {
			j++
		}
		season := missing[i].SeasonNumber
		b := searchBatch{item: base, eps: missing[i:j]}
		b.item.Target, b.item.Season = seasonTargetKey(s.ID, season), &season
		out = append(out, b)
		i = j
	}
	return out
}

func (x *run) search(ctx context.Context, b searchBatch) error {
	eps := b.eps[:min(len(b.eps), x.s.MaxSearches-x.c.Searched)]
	ids := make([]int, 0, len(eps))
	for _, e := range eps {
		ids = append(ids, e.ID)
	}
	item := b.item
	item.Episodes = numbers(eps, b.anime)
	x.report("episode pass", item.SeriesTitle+" "+item.Episodes)
	x.c.Searched += len(ids)
	if x.dry {
		item.Result = resultWouldSearch
		return x.addItem(ctx, &item)
	}
	if !x.lastSearch.IsZero() {
		if wait := searchGap - x.Now().Sub(x.lastSearch); wait > 0 {
			if err := x.Sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	if err := x.guard(ctx); err != nil {
		return err
	}
	item.Result = resultSearched
	if err := x.addItem(ctx, &item); err != nil {
		return err
	}
	cmdID, err := x.Sonarr.SearchEpisodes(ctx, ids)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		x.lastSearch = x.Now()
		x.logger().WarnContext(ctx, "sweep episode search failed", "target", item.Target, "err", err)
		return x.DB.UpdateSweepItem(ctx, item.ID, resultFailed, "")
	}
	err = x.waitCommand(ctx, cmdID)
	x.lastSearch = x.Now()
	return err
}

func (x *run) waitCommand(ctx context.Context, id int) error {
	for range int(commandWait / commandPoll) {
		if err := x.Sleep(ctx, commandPoll); err != nil {
			return err
		}
		status, err := retry(ctx, x, false, func() (string, error) { return x.Sonarr.Command(ctx, id) })
		if err != nil {
			return err
		}
		if status != "queued" && status != "started" {
			return nil
		}
	}
	return nil
}

func (x *run) cleanup(ctx context.Context) error {
	if len(x.imported) == 0 {
		return nil
	}
	x.report("leftover cleanup", "")
	queue, err := retry(ctx, x, false, func() ([]client.QueueDetail, error) { return x.Sonarr.QueueDetails(ctx) })
	if err != nil {
		return err
	}
	for _, dlID := range x.imported {
		var recs []client.QueueDetail
		for _, q := range queue {
			if strings.EqualFold(q.DownloadID, dlID) {
				recs = append(recs, q)
			}
		}
		if len(recs) == 0 || slices.ContainsFunc(recs, func(q client.QueueDetail) bool { return !IsLeftover(q) }) {
			continue
		}
		if err = x.guard(ctx); err != nil {
			return err
		}
		err = x.Sonarr.RemoveQueueItem(ctx, recs[0].ID, false, false, false)
		if err != nil && !errors.Is(err, client.ErrNotFound) {
			if ctx.Err() != nil {
				return err
			}
			x.logger().WarnContext(ctx, "sweep leftover cleanup failed", "download", dlID, "err", err)
		}
	}
	return nil
}

func (x *run) attempts(
	ctx context.Context,
	base db.SweepItem,
	cands []client.Release,
	maxTries int,
) (client.Release, bool, error) {
	tries := 0
	for _, rel := range cands {
		if tries == maxTries {
			break
		}
		key := NormTitle(rel.Title)
		if x.tried[key] || x.refused[key] {
			continue
		}
		x.tried[key] = true
		item := base
		if tries > 0 {
			item.LookupResults, item.LookupMS = nil, nil
		}
		tries++
		handled, err := x.grab(ctx, item, rel)
		if err != nil || handled {
			return rel, handled, err
		}
	}
	if tries > 0 {
		return client.Release{}, false, nil
	}
	base.Result = resultNoPack
	x.c.NoPack++
	return client.Release{}, false, x.addItem(ctx, &base)
}

func (x *run) grab(ctx context.Context, item db.SweepItem, rel client.Release) (bool, error) {
	if x.c.Grabs >= x.s.MaxGrabs {
		return false, errCapReached
	}
	item.ReleaseTitle, item.Indexer = rel.Title, rel.Indexer
	if x.dry {
		x.c.Grabs++
		item.Result = resultWouldGrab
		return true, x.addItem(ctx, &item)
	}
	if !x.lastGrab.IsZero() {
		if wait := grabGap - x.Now().Sub(x.lastGrab); wait > 0 {
			if err := x.Sleep(ctx, wait); err != nil {
				return false, err
			}
		}
	}
	if err := x.guard(ctx); err != nil {
		return false, err
	}
	item.Result = resultGrabbing
	if err := x.addItem(ctx, &item); err != nil {
		return false, err
	}
	x.c.Grabs++
	x.lastGrab = x.Now()
	since := x.lastGrab.Add(-historySkew)
	dlID := ""
	if perr := x.Sonarr.GrabRelease(ctx, rel.GUID, rel.IndexerID); perr != nil {
		if err := x.Sleep(ctx, postCheckDelay); err != nil {
			return false, err
		}
		hist, err := retry(ctx, x, false, func() ([]client.HistoryRecord, error) {
			return x.Sonarr.HistorySince(ctx, since)
		})
		if err != nil {
			return false, err
		}
		if dlID = GrabbedDownloadID(hist, rel.Title); dlID == "" {
			return false, x.refuse(ctx, item.ID, rel, perr)
		}
	}
	result, dlID, err := x.wait(ctx, rel.Title, dlID, since)
	if err != nil {
		return false, err
	}
	switch result {
	case resultImported:
		x.c.Imported++
		x.imported = append(x.imported, dlID)
	case resultFailed:
		x.c.Failed++
	case resultDownloading:
		x.c.Downloading++
	}
	return result == resultImported || result == resultDownloading, x.DB.UpdateSweepItem(ctx, item.ID, result, dlID)
}

func (x *run) wait(ctx context.Context, title, dlID string, since time.Time) (string, string, error) {
	for range int(grabWait / grabPoll) {
		if err := x.Sleep(ctx, grabPoll); err != nil {
			return "", dlID, err
		}
		hist, err := retry(ctx, x, false, func() ([]client.HistoryRecord, error) {
			return x.Sonarr.HistorySince(ctx, since)
		})
		if err != nil {
			return "", dlID, err
		}
		if dlID == "" {
			if dlID = GrabbedDownloadID(hist, title); dlID == "" {
				continue
			}
		}
		queue, err := retry(ctx, x, false, func() ([]client.QueueDetail, error) { return x.Sonarr.QueueDetails(ctx) })
		if err != nil {
			return "", dlID, err
		}
		outcome := Outcome(hist, dlID)
		for _, q := range queue {
			if strings.EqualFold(q.DownloadID, dlID) && IsArchive(q) {
				return resultArchive, dlID, x.dropArchive(ctx, q.ID, outcome == resultImported)
			}
		}
		if outcome != "" {
			return outcome, dlID, nil
		}
	}
	return resultDownloading, dlID, nil
}

func (x *run) dropArchive(ctx context.Context, queueID int, imported bool) error {
	if err := x.guard(ctx); err != nil {
		return err
	}
	err := x.Sonarr.RemoveQueueItem(ctx, queueID, !imported, true, true)
	if errors.Is(err, client.ErrNotFound) {
		return nil
	}
	return err
}

func (x *run) refuse(ctx context.Context, itemID int64, rel client.Release, perr error) error {
	reason := perr.Error()
	if se, ok := errors.AsType[*client.HTTPStatusError](perr); ok {
		reason = fmt.Sprintf("%d %s", se.Status, se.Body)
		if se.Code != "" {
			reason += " code=" + se.Code
		}
	}
	key := NormTitle(rel.Title)
	x.refused[key] = true
	x.c.Refused++
	err := x.DB.AddSweepRefused(ctx, &db.SweepRefused{
		TitleKey: key, ReleaseTitle: rel.Title, Indexer: rel.Indexer, RefusedAt: x.Now(), Reason: reason,
	})
	if err != nil {
		return err
	}
	return x.DB.UpdateSweepItem(ctx, itemID, resultRefused, "")
}

func (x *run) guard(ctx context.Context) error {
	paused, err := x.DB.IsAutonomousPaused(ctx)
	if err != nil {
		return err
	}
	if paused {
		return ErrPaused
	}
	cfg, err := retry(ctx, x, false, func() (client.DownloadClientConfig, error) {
		return x.Sonarr.DownloadClientConfig(ctx)
	})
	if err != nil {
		return err
	}
	if cfg.AutoRedownloadFailed && cfg.AutoRedownloadFailedFromInteractiveSearch {
		return errRedownload
	}
	return nil
}

func (x *run) addItem(ctx context.Context, it *db.SweepItem) error {
	it.At = x.Now()
	return x.DB.AddSweepItem(ctx, it)
}

func (x *run) report(phase, current string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.progress = &Progress{RunID: x.id, Phase: phase, Current: current, Dry: x.dry, Counts: x.c}
}

func (r *Runner) notifyStart(ctx context.Context, c card.Card) bool {
	if r.Notify == nil {
		return false
	}
	err := r.Notify(ctx, c)
	if err != nil && !errors.Is(err, card.ErrUntracked) {
		r.logger().WarnContext(ctx, "sweep start DM failed", "subject", c.Subject, "err", err)
	}
	return err == nil
}

func (r *Runner) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

func seasonTargetKey(seriesID, season int) string {
	return fmt.Sprintf("s:%d:%d", seriesID, season)
}

func episodeTargetKey(episodeID int) string {
	return fmt.Sprintf("e:%d", episodeID)
}

func byAbsolute(a, b client.Episode) int {
	return a.AbsoluteEpisodeNumber - b.AbsoluteEpisodeNumber
}

func numbers(eps []client.Episode, absolute bool) string {
	nums := make([]string, 0, len(eps))
	for _, e := range eps {
		n := e.EpisodeNumber
		if absolute {
			n = e.AbsoluteEpisodeNumber
		}
		nums = append(nums, strconv.Itoa(n))
	}
	return strings.Join(nums, ",")
}

func retry[T any](ctx context.Context, x *run, release bool, f func() (T, error)) (T, error) {
	start := x.Now()
	for {
		v, err := f()
		if err == nil || ctx.Err() != nil || !retryable(err, release) || x.Now().Sub(start) >= getRetryFor {
			return v, err
		}
		x.logger().WarnContext(ctx, "sweep sonarr call failed, retrying", "err", err)
		if serr := x.Sleep(ctx, getRetryEvery); serr != nil {
			return v, err
		}
	}
}

func retryable(err error, release bool) bool {
	if se, ok := errors.AsType[*client.HTTPStatusError](err); ok {
		if release {
			return se.Status == http.StatusBadGateway || se.Status == http.StatusServiceUnavailable
		}
		return se.Status >= http.StatusInternalServerError
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var ne net.Error
	return !release && errors.As(err, &ne) && ne.Timeout()
}
