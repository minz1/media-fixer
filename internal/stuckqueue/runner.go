package stuckqueue

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

type Action string

const (
	ActionImport Action = "import"
	ActionRegrab Action = "regrab"
	ActionRemove Action = "remove"
)

const (
	maxAutoClears = 20
	dmLimit       = 2000
	dmHeader      = "📦 Stuck downloads need you (Maintenance → Stuck queue):"
	noReason      = "no reason given"
)

type Item struct {
	Download

	Verdict       Verdict
	Age           time.Duration
	Actions       []Action
	ImportRefusal string
	Rejections    []string
}

type AppResult struct {
	App         App
	Configured  bool
	Unreachable bool
	Err         string
	Items       []Item
	Cleared     int
	WouldClear  int
	Waiting     int
}

type Result struct {
	Apps        []AppResult
	DryRun      bool
	MinAgeHours int
	CapHit      bool
	FinishedAt  time.Time
}

type Runner struct {
	Sonarr *client.ArrClient
	Radarr *client.ArrClient
	DB     *db.DB
	Notify func(context.Context, string) error
	Now    func() time.Time
	Log    *slog.Logger
}

type sweepRun struct {
	r      *Runner
	set    Settings
	hold   bool
	clears int
	capHit bool
	fresh  []Item
}

func (r *Runner) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

func (r *Runner) client(app App) *client.ArrClient {
	switch app {
	case AppSonarr:
		return r.Sonarr
	case AppRadarr:
		return r.Radarr
	}
	return nil
}

func (r *Runner) Run(ctx context.Context, set Settings) Result {
	x := &sweepRun{r: r, set: set, hold: set.DryRun || r.paused(ctx)}
	res := Result{DryRun: set.DryRun, MinAgeHours: set.MinAgeHours}
	for _, a := range []App{AppSonarr, AppRadarr} {
		res.Apps = append(res.Apps, x.app(ctx, a))
	}
	res.CapHit = x.capHit
	r.notify(ctx, x.fresh, x.capHit)
	res.FinishedAt = r.Now()
	return res
}

func (r *Runner) paused(ctx context.Context) bool {
	p, err := r.DB.IsAutonomousPaused(ctx)
	if err != nil {
		r.logger().WarnContext(ctx, "stuck-queue pause check failed; not clearing this run", "err", err)
		return true
	}
	return p
}

func (x *sweepRun) app(ctx context.Context, a App) AppResult {
	ar := AppResult{App: a}
	c := x.r.client(a)
	if c == nil {
		return ar
	}
	ar.Configured = true
	entries, err := c.QueueEntries(ctx)
	if err != nil {
		ar.Unreachable, ar.Err = true, err.Error()
		x.r.logger().WarnContext(ctx, "stuck-queue: arr unreachable", "app", a, "err", err)
		return ar
	}
	now := x.r.Now()
	var keep []string
	for _, d := range Group(a, entries) {
		if !d.Stuck() {
			continue
		}
		keep = append(keep, d.DownloadID)
		v := Classify(d)
		row, upErr := x.r.DB.UpsertStuckSeen(ctx, string(a), d.DownloadID, d.Title, string(v), now)
		if upErr != nil {
			x.r.logger().
				WarnContext(ctx, "stuck-queue: record download", "app", a, "download", d.DownloadID, "err", upErr)
			continue
		}
		age := now.Sub(row.FirstSeenAt)
		if age < x.set.MinAge() {
			ar.Waiting++
			continue
		}
		x.handle(ctx, c, &ar, Item{Download: d, Verdict: v, Age: age}, entries, row.NotifiedAt == nil)
	}
	if err = x.r.DB.PruneStuckGone(ctx, string(a), keep); err != nil {
		x.r.logger().WarnContext(ctx, "stuck-queue: prune", "app", a, "err", err)
	}
	return ar
}

func (x *sweepRun) handle(
	ctx context.Context, c *client.ArrClient, ar *AppResult, it Item, queue []client.QueueEntry, isNew bool,
) {
	var guardErr error
	switch it.Verdict {
	case VerdictClear:
		if x.clear(ctx, c, ar, it) {
			return
		}
	case VerdictGap:
		if !it.MaybeHasFile() {
			guardErr = x.guard(ctx, c, &it, queue)
		}
	case VerdictUnmapped:
	}
	it.Actions = offered(it.Download, it.Verdict, guardErr)
	ar.Items = append(ar.Items, it)
	if isNew && it.Verdict != VerdictClear {
		x.fresh = append(x.fresh, it)
	}
}

func (x *sweepRun) clear(ctx context.Context, c *client.ArrClient, ar *AppResult, it Item) bool {
	if x.hold {
		ar.WouldClear++
		return false
	}
	if x.clears >= maxAutoClears {
		x.capHit = true
		ar.WouldClear++
		return false
	}
	if err := c.RemoveQueueItem(ctx, it.Records[0].ID, false, false, true); err != nil {
		x.r.logger().WarnContext(ctx, "stuck-queue: clear failed", "app", it.App, "download", it.DownloadID, "err", err)
		return false
	}
	x.clears++
	ar.Cleared++
	x.r.logger().InfoContext(ctx, "stuck-queue: cleared leftover download",
		"app", it.App, "download", it.DownloadID, "title", it.Title)
	return true
}

func (x *sweepRun) guard(ctx context.Context, c *client.ArrClient, it *Item, queue []client.QueueEntry) error {
	cands, err := c.ManualImportCandidates(ctx, it.DownloadID)
	if err != nil {
		err = refuse("couldn't read the import candidates: %v", err)
	} else {
		err = ImportGuard(it.Download, cands, queue)
	}
	for _, cand := range cands {
		for _, rj := range cand.Rejections {
			if !slices.Contains(it.Rejections, rj) {
				it.Rejections = append(it.Rejections, rj)
			}
		}
	}
	if err != nil {
		it.ImportRefusal = err.Error()
	}
	return err
}

func offered(d Download, v Verdict, guardErr error) []Action {
	switch v {
	case VerdictClear, VerdictUnmapped:
		return []Action{ActionRemove}
	case VerdictGap:
		if d.MaybeHasFile() {
			return []Action{ActionRemove}
		}
		if guardErr == nil {
			return []Action{ActionImport, ActionRegrab, ActionRemove}
		}
		return []Action{ActionRegrab, ActionRemove}
	}
	return nil
}

func (r *Runner) notify(ctx context.Context, items []Item, capHit bool) {
	if len(items) == 0 && !capHit {
		return
	}
	if r.Notify == nil {
		return
	}
	if err := r.Notify(ctx, dmText(items, capHit)); err != nil {
		r.logger().WarnContext(ctx, "stuck-queue: owner DM failed; will retry next run", "err", err)
		return
	}
	now := r.Now()
	for _, it := range items {
		if err := r.DB.MarkStuckNotified(ctx, string(it.App), it.DownloadID, now); err != nil {
			r.logger().WarnContext(ctx, "stuck-queue: mark notified", "download", it.DownloadID, "err", err)
		}
	}
}

func dmText(items []Item, capHit bool) string {
	capLine := fmt.Sprintf("Stopped after clearing %d this run.", maxAutoClears)
	if len(items) == 0 {
		return "📦 Stuck-queue sweep: " + capLine
	}
	tail := ""
	if capHit {
		tail = "\n" + capLine
	}
	var b strings.Builder
	b.WriteString(dmHeader)
	for i, it := range items {
		line := "\n• " + it.App.Name() + " — " + it.Title + " — " + reason(it)
		more := fmt.Sprintf("\n…and %d more", len(items)-i)
		if b.Len()+len(line)+len(more)+len(tail) > dmLimit {
			b.WriteString(more)
			break
		}
		b.WriteString(line)
	}
	b.WriteString(tail)
	return b.String()
}

func (a App) Name() string {
	switch a {
	case AppSonarr:
		return "Sonarr"
	case AppRadarr:
		return "Radarr"
	}
	return string(a)
}

func reason(it Item) string {
	movie := it.App == AppRadarr
	if it.Verdict == VerdictUnmapped {
		if movie {
			return "not matched to any movie"
		}
		return "not matched to any episode"
	}
	missing := slices.DeleteFunc(slices.Clone(it.Records), func(r client.QueueEntry) bool {
		return !r.HasFileKnown || r.HasFile
	})
	switch {
	case len(missing) > 0 && movie:
		return "movie missing — blocked by: " + firstMessage(missing)
	case len(missing) > 0:
		return "episode missing (" + targets(missing) + ") — blocked by: " + firstMessage(missing)
	case slices.ContainsFunc(it.Records, func(r client.QueueEntry) bool { return !r.HasFileKnown }):
		return "can't tell if it has a file — blocked by: " + firstMessage(it.Records)
	case movie:
		return "Radarr has a file for this movie but didn't compare it"
	}
	return "Sonarr has a file for " + it.Targets() + " but didn't compare it"
}

func firstMessage(recs []client.QueueEntry) string {
	for _, r := range recs {
		if len(r.Messages) > 0 {
			return r.Messages[0]
		}
	}
	return noReason
}
