package stuckread

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

const (
	pollInterval = time.Minute
	clearPolls   = 2
	staleAfter   = 3 * pollInterval
)

type Source interface {
	StuckReads(ctx context.Context) (*mediaagentapi.StuckReadsResult, error)
}

type taskKey struct {
	pid, tid int
	start    uint64
}

type Watcher struct {
	src Source
	svc *incident.Service
	db  *db.DB
	log *slog.Logger
	now func() time.Time
	wg  sync.WaitGroup

	prev       map[taskKey]mediaagentapi.StuckReader
	firstStuck map[taskKey]time.Time
	warned     map[string]bool

	mu        sync.Mutex
	failing   bool
	lastGood  time.Time
	okPolls   int
	lastStuck map[string]int
}

func New(src Source, svc *incident.Service, database *db.DB, log *slog.Logger) *Watcher {
	return &Watcher{
		src: src, svc: svc, db: database, log: log, now: time.Now,
		prev:       map[taskKey]mediaagentapi.StuckReader{},
		firstStuck: map[taskKey]time.Time{},
		warned:     map[string]bool{},
		lastStuck:  map[string]int{},
	}
}

func (w *Watcher) Start(ctx context.Context) {
	w.wg.Go(func() {
		tick := time.NewTicker(pollInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				w.Poll(ctx)
			}
		}
	})
}

func (w *Watcher) Wait() { w.wg.Wait() }

func (w *Watcher) Clear(title string) incident.ReadState {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failing || w.okPolls < clearPolls || w.now().Sub(w.lastGood) > staleAfter {
		return incident.ReadUnknown
	}
	if w.okPolls-w.lastStuck[title] >= clearPolls {
		return incident.ReadClear
	}
	return incident.ReadStuck
}

func (w *Watcher) Poll(ctx context.Context) {
	res, err := w.src.StuckReads(ctx)
	if err != nil {
		w.mu.Lock()
		wasFailing := w.failing
		w.failing = true
		w.mu.Unlock()
		if !wasFailing {
			w.log.WarnContext(ctx, "stuck-read poll failing", "error", err)
		}
		return
	}
	now := w.now()
	stuck := w.track(res.Readers, now)
	w.recordClear(ctx, stuck, now)

	timers, _ := incident.LoadTimers(ctx, w.db)
	limit := time.Duration(timers.StuckReadMinutes) * time.Minute
	warned := map[string]bool{}
	for _, title := range slices.Sorted(maps.Keys(stuck)) {
		rs := stuck[title]
		if len(rs) >= timers.StuckReadTasks || w.oldest(rs, now) >= limit {
			w.handleFinding(ctx, title, rs, now, warned)
		}
	}
	w.warned = warned
}

func (w *Watcher) track(readers []mediaagentapi.StuckReader, now time.Time) map[string][]mediaagentapi.StuckReader {
	cur := make(map[taskKey]mediaagentapi.StuckReader, len(readers))
	stuck := map[string][]mediaagentapi.StuckReader{}
	for _, r := range readers {
		k := taskKey{r.PID, r.TID, r.Start}
		cur[k] = r
		if p, ok := w.prev[k]; ok && p.Switches == r.Switches {
			if _, seen := w.firstStuck[k]; !seen {
				w.firstStuck[k] = now
			}
			title := Title(r.Path)
			stuck[title] = append(stuck[title], r)
		} else {
			delete(w.firstStuck, k)
		}
	}
	for k := range w.firstStuck {
		if _, ok := cur[k]; !ok {
			delete(w.firstStuck, k)
		}
	}
	w.prev = cur
	return stuck
}

func (w *Watcher) recordClear(ctx context.Context, stuck map[string][]mediaagentapi.StuckReader, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failing {
		w.failing = false
		w.log.InfoContext(ctx, "stuck-read poll recovered")
	}
	w.lastGood = now
	w.okPolls++
	for title, last := range w.lastStuck {
		if _, now := stuck[title]; !now && last == w.okPolls-1 {
			w.log.InfoContext(ctx, "stuck read cleared", "title", title)
		}
		if w.okPolls-last >= clearPolls {
			delete(w.lastStuck, title)
		}
	}
	for title := range stuck {
		w.lastStuck[title] = w.okPolls
	}
}

func (w *Watcher) oldest(rs []mediaagentapi.StuckReader, now time.Time) time.Duration {
	var age time.Duration
	for _, r := range rs {
		age = max(age, now.Sub(w.firstStuck[taskKey{r.PID, r.TID, r.Start}]))
	}
	return age
}

func (w *Watcher) handleFinding(
	ctx context.Context, title string, rs []mediaagentapi.StuckReader, now time.Time, warned map[string]bool,
) {
	inc, err := w.db.FindUnresolvedByTitle(ctx, title)
	switch {
	case errors.Is(err, db.ErrNotFound):
		w.open(ctx, title, rs, now)
	case err != nil:
		w.log.WarnContext(ctx, "look up stuck-read incident", "title", title, "error", err)
	case inc.What != agent.WhatStuckRead:
		if !lifecycle.Meta(inc.Status).Active {
			w.open(ctx, title, rs, now)
		}
	case inc.Status == db.StatusAgentFixed:
		w.retryFixed(ctx, inc, warned)
	}
}

func (w *Watcher) open(ctx context.Context, title string, rs []mediaagentapi.StuckReader, now time.Time) {
	_, err := w.svc.Handle(ctx, &incident.Report{
		Source: "media-agent", ReportedBy: "media-agent", What: agent.WhatStuckRead,
		Title: title, Details: w.details(rs, now),
	})
	if err != nil {
		w.log.WarnContext(ctx, "open stuck-read incident", "title", title, "error", err)
	}
}

func (w *Watcher) retryFixed(ctx context.Context, inc *db.Incident, warned map[string]bool) {
	if inc.StuckReadReran {
		warned[inc.ID] = true
		if !w.warned[inc.ID] {
			w.log.WarnContext(ctx, "stuck read again after a rerun", "incident", inc.ID, "title", inc.Title)
		}
		return
	}
	open, err := w.db.CountOpenIncidents(ctx)
	if err != nil {
		w.log.WarnContext(ctx, "count open incidents", "error", err)
		return
	}
	if open >= incident.SystematicIncidentThreshold {
		return
	}
	if err = w.svc.SystemRerun(ctx, inc.ID, inc.Generation); errors.Is(err, card.ErrRestarting) {
		w.log.InfoContext(ctx, "rerun stuck-read incident refused, shutting down", "incident", inc.ID, "error", err)
		return
	}
	if err != nil {
		w.log.WarnContext(ctx, "rerun stuck-read incident", "incident", inc.ID, "error", err)
		return
	}
}

func (w *Watcher) details(rs []mediaagentapi.StuckReader, now time.Time) string {
	var b strings.Builder
	for _, r := range rs {
		age := now.Sub(w.firstStuck[taskKey{r.PID, r.TID, r.Start}]).Round(time.Second)
		fmt.Fprintf(&b, "%s: %s pid=%d tid=%d stuck %s via %s\n", r.Path, r.Comm, r.PID, r.TID, age, r.Via)
	}
	return b.String()
}
