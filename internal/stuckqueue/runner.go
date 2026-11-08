package stuckqueue

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
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
	maxAutoClears  = 20
	releaseTimeout = 5 * time.Second
	dmHeader       = "Stuck downloads need you"
	noReason       = "no reason given"
)

type Item struct {
	Download

	Verdict       Verdict
	State         State
	Acted         *Acted
	Age           time.Duration
	Due           time.Time
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
	Waiting     []Item
}

type Result struct {
	Apps        []AppResult
	DryRun      bool
	Paused      bool
	MinAgeHours int
	CapHit      bool
	FinishedAt  time.Time
}

type Runner struct {
	Sonarr *client.ArrClient
	Radarr *client.ArrClient
	DB     *db.DB
	Notify func(context.Context, card.Card) error
	Now    func() time.Time
	Log    *slog.Logger
}

type sweepRun struct {
	r      *Runner
	set    Settings
	hold   bool
	now    time.Time
	clears int
	capHit bool
	due    []Item
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

func allApps() []App { return []App{AppSonarr, AppRadarr} }

func (r *Runner) Run(ctx context.Context, set Settings) Result {
	paused := !set.DryRun && r.paused(ctx)
	x := &sweepRun{r: r, set: set, hold: set.DryRun || paused, now: r.Now()}
	res := Result{DryRun: set.DryRun, Paused: paused, MinAgeHours: set.MinAgeHours}
	for _, a := range allApps() {
		res.Apps = append(res.Apps, x.app(ctx, a))
	}
	res.CapHit = x.capHit
	r.notify(ctx, x.due, x.capHit)
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

type appRun struct {
	app       App
	c         *client.ArrClient
	queue     []client.QueueEntry
	stored    map[string]Snapshot
	downloads []Download
	observed  map[string]Item
	events    map[Key]Event
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
	rows, err := x.r.DB.ListStuck(ctx, string(a))
	if err != nil {
		ar.Unreachable, ar.Err = true, err.Error()
		x.r.logger().WarnContext(ctx, "stuck-queue: read rows", "app", a, "err", err)
		return ar
	}
	run := &appRun{
		app: a, c: c, queue: entries, stored: make(map[string]Snapshot, len(rows)),
		observed: map[string]Item{}, events: map[Key]Event{},
	}
	for _, row := range rows {
		run.stored[row.DownloadID] = fromRow(row)
	}
	x.observe(ctx, run)
	final, effs, committed := x.commit(ctx, run)
	removed := map[string]bool{}
	if committed {
		x.clearAll(ctx, run, &ar, effs, final, removed)
	}
	x.list(&ar, run, final, removed, committed)
	return ar
}

func (x *sweepRun) observe(ctx context.Context, run *appRun) {
	present := map[string]bool{}
	for _, d := range Group(run.app, run.queue) {
		if !d.Stuck() {
			continue
		}
		present[d.DownloadID] = true
		run.downloads = append(run.downloads, d)
		it, offer := x.inspect(ctx, run, d)
		run.observed[d.DownloadID] = it
		run.events[Key{App: run.app, DownloadID: d.DownloadID}] = Event{
			Kind: EvSeen, At: x.now, MinAge: x.set.MinAge(), Hold: x.hold,
			Obs: Observation{FP: d.TargetsFP(), Verdict: it.Verdict, Offered: offer},
		}
	}
	for id := range run.stored {
		if !present[id] {
			run.events[Key{App: run.app, DownloadID: id}] = Event{Kind: EvMissed, At: x.now}
		}
	}
}

func (x *sweepRun) inspect(ctx context.Context, run *appRun, d Download) (Item, Offer) {
	it := Item{Download: d, Verdict: Classify(d)}
	var guardErr error
	if it.Verdict == VerdictGap && !d.MaybeHasFile() {
		guardErr = refuse("the import guard did not run for this download")
		if cur, ok := run.stored[d.DownloadID]; ok && x.guardable(cur) {
			guardErr = x.guard(ctx, run.c, &it, run.queue)
		}
	}
	return it, offerOf(offered(d, it.Verdict, guardErr))
}

func (x *sweepRun) guardable(cur Snapshot) bool {
	if cur.State == StateImportInflight || cur.State == StateActed {
		return false
	}
	return !x.now.Before(cur.FirstSeen.Add(x.set.MinAge()))
}

func offerOf(actions []Action) Offer {
	parts := make([]string, 0, len(actions))
	for _, a := range actions {
		parts = append(parts, string(a))
	}
	return Offer(strings.Join(parts, " "))
}

func (x *sweepRun) commit(ctx context.Context, run *appRun) (map[string]Snapshot, []Effect, bool) {
	final := map[string]Snapshot{}
	if len(run.events) == 0 {
		return final, nil, true
	}
	snaps, effs, err := x.r.apply(ctx, run.app, run.events)
	if err != nil {
		x.r.logger().WarnContext(ctx, "stuck-queue: record downloads", "app", run.app, "err", err)
		for k, ev := range run.events {
			before := Snapshot{Key: k, State: StateUntracked}
			if cur, ok := run.stored[k.DownloadID]; ok {
				before = cur
			}
			after, _, nextErr := Next(before, ev)
			if nextErr != nil {
				after = before
			}
			final[k.DownloadID] = after
		}
		return final, nil, false
	}
	for _, s := range snaps {
		final[s.Key.DownloadID] = s
	}
	return final, effs, true
}

func (x *sweepRun) clearAll(
	ctx context.Context, run *appRun, ar *AppResult, effs []Effect, final map[string]Snapshot, removed map[string]bool,
) {
	byID := map[string]Download{}
	for _, d := range run.downloads {
		byID[d.DownloadID] = d
	}
	results := map[Key]Event{}
	for _, e := range effs {
		ce, ok := e.(EffClearDownload)
		if !ok {
			continue
		}
		if x.clears >= maxAutoClears {
			x.capHit = true
			ar.WouldClear++
			continue
		}
		ev := x.clearOne(ctx, run.c, byID[ce.Key.DownloadID])
		results[ce.Key] = ev
		if ev.Kind == EvClearDone {
			removed[ce.Key.DownloadID] = true
			ar.Cleared++
		}
	}
	if len(results) == 0 {
		return
	}
	snaps, _, err := x.r.apply(ctx, run.app, results)
	if err != nil {
		x.r.logger().WarnContext(ctx, "stuck-queue: record clears", "app", run.app, "err", err)
		return
	}
	for _, s := range snaps {
		final[s.Key.DownloadID] = s
	}
}

func (x *sweepRun) clearOne(ctx context.Context, c *client.ArrClient, d Download) Event {
	err := c.RemoveQueueItem(ctx, d.Records[0].ID, false, false, true)
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		x.r.logger().WarnContext(ctx, "stuck-queue: clear failed", "app", d.App, "download", d.DownloadID, "err", err)
		return Event{Kind: EvClearFailed, At: x.now}
	}
	x.clears++
	x.r.logger().InfoContext(ctx, "stuck-queue: cleared leftover download",
		"app", d.App, "download", d.DownloadID, "title", d.Title)
	return Event{Kind: EvClearDone, At: x.now}
}

func (x *sweepRun) list(
	ar *AppResult, run *appRun, final map[string]Snapshot, removed map[string]bool, committed bool,
) {
	for _, d := range run.downloads {
		s := final[d.DownloadID]
		if s.State == StateUntracked || s.State == StateCleared || removed[d.DownloadID] {
			continue
		}
		it := run.observed[d.DownloadID]
		it.State, it.Acted, it.Age, it.Actions = s.State, s.Acted, x.now.Sub(s.FirstSeen), s.Offered.Actions()
		if s.State == StateWaiting {
			it.Due = s.FirstSeen.Add(x.set.MinAge())
			ar.Waiting = append(ar.Waiting, it)
			continue
		}
		if s.State == StateHeld && x.hold {
			ar.WouldClear++
		}
		ar.Items = append(ar.Items, it)
		if committed && s.State == StateDue {
			x.due = append(x.due, it)
		}
	}
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

func (r *Runner) notify(ctx context.Context, due []Item, capHit bool) {
	if (len(due) == 0 && !capHit) || r.Notify == nil {
		return
	}
	slices.SortStableFunc(due, func(a, b Item) int { return cmp.Compare(b.Age, a.Age) })
	c, shown := stuckCard(due, capHit)
	claimAt := r.Now()
	claimed := r.claim(ctx, due[:shown], claimAt)
	if len(due) > 0 && len(claimed) == 0 {
		return
	}
	if len(claimed) < shown {
		var fit int
		c, fit = stuckCard(claimed, capHit)
		if fit < len(claimed) {
			r.release(ctx, claimed[fit:], claimAt)
			claimed = claimed[:fit]
		}
	}
	if err := r.Notify(ctx, c); err != nil && !errors.Is(err, card.ErrUntracked) {
		r.logger().WarnContext(ctx, "stuck-queue: owner DM failed; will retry next run", "err", err)
		r.release(ctx, claimed, claimAt)
	}
}

func itemEvents(items []Item, app App, ev Event) map[Key]Event {
	out := map[Key]Event{}
	for _, it := range items {
		if it.App == app {
			out[Key{App: app, DownloadID: it.DownloadID}] = ev
		}
	}
	return out
}

func (r *Runner) claim(ctx context.Context, items []Item, at time.Time) []Item {
	won := map[Key]bool{}
	for _, a := range allApps() {
		events := itemEvents(items, a, Event{Kind: EvClaimed, At: at})
		if len(events) == 0 {
			continue
		}
		snaps, _, err := r.apply(ctx, a, events)
		if err != nil {
			r.logger().WarnContext(ctx, "stuck-queue: claim failed; sending nothing for this app", "app", a, "err", err)
			continue
		}
		for _, s := range snaps {
			if s.State == StateNotified && s.NotifiedAt.Equal(at) {
				won[s.Key] = true
			}
		}
	}
	return slices.DeleteFunc(slices.Clone(items), func(it Item) bool {
		return !won[Key{App: it.App, DownloadID: it.DownloadID}]
	})
}

func (r *Runner) release(ctx context.Context, items []Item, at time.Time) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	lost := 0
	for _, a := range allApps() {
		events := itemEvents(items, a, Event{Kind: EvNotifyFailed, At: at, ClaimAt: at})
		if len(events) == 0 {
			continue
		}
		if _, _, err := r.apply(rctx, a, events); err != nil {
			lost += len(events)
			r.logger().ErrorContext(rctx, "stuck-queue: release failed", "app", a, "err", err)
		}
	}
	if lost > 0 {
		r.logger().ErrorContext(rctx, "stuck-queue: notification lost", "count", lost)
	}
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
		return r.File != client.FileMissing
	})
	switch {
	case len(missing) > 0 && movie:
		return "movie missing — blocked by: " + firstMessage(missing)
	case len(missing) > 0:
		return "episode missing (" + targets(missing) + ") — blocked by: " + firstMessage(missing)
	case slices.ContainsFunc(it.Records, func(r client.QueueEntry) bool { return r.File == client.FileUnknown }):
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
