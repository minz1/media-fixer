package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
)

type Sender interface {
	NotifyOwner(ctx context.Context, c card.Card) error
	NotifyUser(ctx context.Context, userID string, c card.Card) error
	SeerrComment(ctx context.Context, issueID, text string) error
	SeerrResolve(ctx context.Context, issueID string) error
}

const (
	giveUpAfter = 24 * time.Hour
	tick        = 30 * time.Second
	batch       = 100
)

var errUndeliverable = errors.New("undeliverable")

func backoff(attempts int) time.Duration {
	steps := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}
	return steps[min(attempts, len(steps)-1)]
}

type Dispatcher struct {
	db     *db.DB
	s      Sender
	log    *slog.Logger
	wake   chan struct{}
	now    func() time.Time
	mu     sync.Mutex
	failed func(ctx context.Context, r db.OutboxRow, now time.Time, err error) bool
	gaveUp func(ctx context.Context, r db.OutboxRow, now time.Time)
}

func (d *Dispatcher) OnFailed(f func(ctx context.Context, r db.OutboxRow, now time.Time, err error) bool) {
	d.failed = f
}

func (d *Dispatcher) OnGiveUp(f func(ctx context.Context, r db.OutboxRow, now time.Time)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gaveUp = f
}

func (d *Dispatcher) Flush(ctx context.Context, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for range 3 {
		if !d.drainAt(ctx, now) {
			return
		}
	}
}

func New(d *db.DB, s Sender, log *slog.Logger) *Dispatcher {
	return &Dispatcher{db: d, s: s, log: log, wake: make(chan struct{}, 1), now: time.Now}
}

func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	var pruned time.Time
	for {
		now := d.now()
		if now.Sub(pruned) >= 24*time.Hour {
			pruned = now
			if n, err := d.db.PruneOutbox(ctx, now); err != nil {
				d.log.WarnContext(ctx, "outbox prune", "error", err)
			} else if n > 0 {
				d.log.InfoContext(ctx, "outbox pruned", "rows", n)
			}
		}
		d.Flush(ctx, now)
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-t.C:
		}
	}
}

func (d *Dispatcher) drainAt(ctx context.Context, now time.Time) bool {
	again := false
	wctx := context.WithoutCancel(ctx)
	rows, err := d.db.DueOutbox(ctx, now, batch)
	if err != nil {
		d.log.WarnContext(ctx, "outbox read", "error", err)
		return false
	}
	blocked := map[string]bool{}
	for _, r := range rows {
		if blocked[r.Target] || ctx.Err() != nil {
			continue
		}
		if now.Sub(r.CreatedAt) >= giveUpAfter {
			d.giveUp(wctx, r, now, "undelivered for 24 h")
			continue
		}
		err = d.send(ctx, r)
		switch {
		case errors.Is(err, errUndeliverable):
			d.giveUp(wctx, r, now, err.Error())
		case err != nil && !errors.Is(err, card.ErrUntracked):
			blocked[r.Target] = true
			d.log.WarnContext(ctx, "outbox send failed", "id", r.ID, "target", r.Target, "kind", r.Kind,
				"attempts", r.Attempts+1, "error", err)
			next := now.Add(min(backoff(r.Attempts), r.CreatedAt.Add(giveUpAfter).Sub(now)))
			if merr := d.db.MarkOutboxFailed(wctx, r.ID, next, err.Error()); merr != nil {
				d.log.WarnContext(ctx, "outbox mark failed", "id", r.ID, "error", merr)
			}
			if d.failed != nil && d.failed(wctx, r, now, err) {
				again = true
			}
		default:
			if merr := d.db.MarkOutboxSent(wctx, r.ID, now); merr != nil {
				blocked[r.Target] = true
				d.log.WarnContext(ctx, "outbox mark sent", "id", r.ID, "error", merr)
			}
		}
	}
	return again
}

func (d *Dispatcher) giveUp(ctx context.Context, r db.OutboxRow, now time.Time, why string) {
	d.log.ErrorContext(ctx, "outbox gave up", "id", r.ID, "key", r.Key, "target", r.Target, "kind", r.Kind,
		"attempts", r.Attempts, "reason", why)
	if d.gaveUp != nil {
		d.gaveUp(ctx, r, now)
	}
	if err := d.db.GiveUpOutbox(ctx, r.ID, now); err != nil {
		d.log.WarnContext(ctx, "outbox give up", "id", r.ID, "error", err)
	}
}

func (d *Dispatcher) send(ctx context.Context, r db.OutboxRow) error {
	scheme, id, _ := strings.Cut(r.Target, ":")
	switch {
	case r.Kind == "seerr_comment" && scheme == "seerr":
		return d.s.SeerrComment(ctx, id, r.Body)
	case r.Kind == "seerr_resolve" && scheme == "seerr":
		return d.s.SeerrResolve(ctx, id)
	case r.Kind != "card":
		return fmt.Errorf("%w: kind %q", errUndeliverable, r.Kind)
	}
	var c card.Card
	if err := json.Unmarshal([]byte(r.Body), &c); err != nil {
		return fmt.Errorf("%w: card body: %w", errUndeliverable, err)
	}
	switch scheme {
	case "owner":
		return d.s.NotifyOwner(ctx, c)
	case "discord":
		return d.s.NotifyUser(ctx, id, c)
	}
	return fmt.Errorf("%w: target %q", errUndeliverable, r.Target)
}
