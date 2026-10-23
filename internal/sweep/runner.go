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

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	keepRuns       = 50
	seasonAttempts = 4
	grabGap        = 60 * time.Second
	grabPoll       = 30 * time.Second
	grabWait       = 7 * time.Minute
	postCheckDelay = 30 * time.Second
	historySkew    = 5 * time.Minute
	getRetryEvery  = 20 * time.Second
	getRetryFor    = 10 * time.Minute
	refusedMemory  = 7 * 24 * time.Hour
	noPackMemory   = 30 * 24 * time.Hour

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
)

var (
	errPaused     = errors.New("paused")
	errRedownload = errors.New("autoRedownloadFailedFromInteractiveSearch is on")
	errCapReached = errors.New("cap reached")
)

type Runner struct {
	Sonarr *client.ArrClient
	DB     *db.DB
	Notify func(context.Context, string) error
	Now    func() time.Time
	Sleep  func(context.Context, time.Duration) error
	Log    *slog.Logger

	mu       sync.Mutex
	progress *Progress
}

type Progress struct {
	Phase, Current string
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

	id       int64
	dry      bool
	s        Settings
	c        db.SweepCounts
	refused  map[string]bool
	skip     map[string]bool
	queued   map[int]bool
	tried    map[string]bool
	imported []string
	lastGrab time.Time
}

type seasonTarget struct {
	series client.Series
	season int
	eps    []client.Episode
}

func (r *Runner) Run(ctx context.Context, trigger string, dry bool, s Settings) (int64, error) {
	if err := r.DB.PruneSweepRuns(ctx, keepRuns); err != nil {
		return 0, err
	}
	start := r.Now()
	id, err := r.DB.CreateSweepRun(ctx, trigger, dry, start)
	if err != nil {
		return 0, err
	}
	x := &run{
		Runner: r, id: id, dry: dry, s: s, tried: map[string]bool{},
		c: db.SweepCounts{MaxGrabs: s.MaxGrabs, MaxLookups: s.MaxLookups, MaxSearches: s.MaxSearches},
	}
	x.report("preflight", "")
	err = x.sweep(ctx)
	r.mu.Lock()
	r.progress = nil
	r.mu.Unlock()

	status, reason := statusDone, ""
	switch {
	case errors.Is(err, errPaused):
		status, err = statusPaused, nil
	case errors.Is(err, errRedownload):
		status, reason, err = statusRefused, errRedownload.Error(), nil
	case ctx.Err() != nil:
		status, reason = statusInterrupted, ctx.Err().Error()
	case err != nil:
		status, reason = statusFailed, err.Error()
	}
	fctx := context.WithoutCancel(ctx)
	if ferr := r.DB.FinishSweepRun(fctx, id, status, reason, x.c, r.Now()); ferr != nil {
		return id, errors.Join(err, ferr)
	}
	if r.Notify != nil {
		if nerr := r.Notify(fctx, x.summary(status, reason, r.Now().Sub(start))); nerr != nil {
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
	var err error
	if x.refused, err = x.DB.RefusedKeys(ctx, now.Add(-refusedMemory)); err != nil {
		return err
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
	if err = x.seasonPass(ctx, series); err != nil && !errors.Is(err, errCapReached) {
		return err
	}
	return nil
}

func (x *run) seasonPass(ctx context.Context, series []client.Series) error {
	x.report("season pass", "")
	var targets []seasonTarget
	for _, s := range series {
		if !s.Monitored || s.SeriesType == "anime" || slices.Contains(x.s.ExcludedSeries, s.ID) {
			continue
		}
		eps, err := retry(ctx, x, false, func() ([]client.Episode, error) {
			return x.Sonarr.GetEpisodes(ctx, s.ID, -1)
		})
		if err != nil {
			return err
		}
		bySeason := map[int][]client.Episode{}
		for _, e := range Missing(eps, x.queued, x.Now()) {
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
	if x.c.Lookups >= x.s.MaxLookups || x.c.Grabs >= x.s.MaxGrabs {
		return errCapReached
	}
	if err := x.guard(ctx); err != nil {
		return err
	}
	x.report("season pass", fmt.Sprintf("%s S%02d", t.series.Title, t.season))
	nums := make([]string, 0, len(t.eps))
	for _, e := range t.eps {
		nums = append(nums, strconv.Itoa(e.EpisodeNumber))
	}
	season := t.season
	item := db.SweepItem{
		RunID: x.id, Target: seasonTargetKey(t.series.ID, t.season), SeriesID: t.series.ID,
		SeriesTitle: t.series.Title, Season: &season, Episodes: strings.Join(nums, ","),
	}
	x.c.Lookups++
	began := x.Now()
	rels, err := retry(ctx, x, true, func() ([]client.Release, error) {
		return x.Sonarr.Releases(ctx, t.series.ID, t.season)
	})
	n, ms := len(rels), int(x.Now().Sub(began).Milliseconds())
	item.LookupResults, item.LookupMS = &n, &ms
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		x.logger().WarnContext(ctx, "sweep lookup failed", "target", item.Target, "err", err)
		item.Result = resultFailed
		return x.addItem(ctx, &item)
	}
	cands := SeasonCandidates(rels, x.refused)
	Rank(cands)
	_, err = x.attempts(ctx, item, cands, seasonAttempts)
	return err
}

func (x *run) attempts(ctx context.Context, base db.SweepItem, cands []client.Release, maxTries int) (bool, error) {
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
			return handled, err
		}
	}
	if tries > 0 {
		return false, nil
	}
	base.Result = resultNoPack
	x.c.NoPack++
	return false, x.addItem(ctx, &base)
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
		return errPaused
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
	x.progress = &Progress{Phase: phase, Current: current, Counts: x.c}
}

func (x *run) summary(status, reason string, took time.Duration) string {
	kind := "Pack sweep"
	if x.dry {
		kind = "Pack sweep dry run"
	}
	if reason != "" {
		status += " (" + reason + ")"
	}
	c := x.c
	return fmt.Sprintf(
		"%s %s: grabs %d/%d, lookups %d/%d, searched %d/%d, imported %d, failed %d, refused %d, "+
			"still downloading %d, took %s. /media/maintenance",
		kind, status, c.Grabs, c.MaxGrabs, c.Lookups, c.MaxLookups, c.Searched, c.MaxSearches,
		c.Imported, c.Failed, c.Refused, c.Downloading, took.Round(time.Second),
	)
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
