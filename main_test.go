package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/server"
)

type recorder struct {
	mu    sync.Mutex
	order []string
}

func (r *recorder) note(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, name)
}

func (r *recorder) fn(name string) func() { return func() { r.note(name) } }

func (r *recorder) step(name string, err error) step {
	return step{name: name, run: func(context.Context) error {
		r.note(name)
		return err
	}}
}

func (r *recorder) has(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.order, name)
}

func (r *recorder) plan(waits, subs []step) shutdownPlan {
	return shutdownPlan{
		intake:  r.fn("intake"),
		cancel:  r.fn("cancel"),
		waits:   waits,
		subs:    subs,
		closeDB: func() error { r.note("db"); return nil },
	}
}

func TestStartup_OrderMaintenanceCardsBot(t *testing.T) {
	t.Parallel()
	var r recorder
	startup(startSteps{
		maintenance: r.fn("maintenance"), cards: r.fn("cards"), bot: r.fn("bot"), intake: r.fn("intake"),
		service: r.fn("service"), recovery: r.fn("recovery"),
	})
	want := []string{"maintenance", "cards", "bot", "intake", "service", "recovery"}
	if !slices.Equal(r.order, want) {
		t.Errorf("startup order = %v, want %v", r.order, want)
	}
}

func TestA10_11_ShutdownClosesSubscriptionsAfterDrain(t *testing.T) {
	t.Parallel()
	var r recorder
	p := r.plan(
		[]step{r.step("wait:discord", nil), r.step("wait:incident", nil)},
		[]step{r.step("sub:discord", nil), r.step("sub:journal", nil)},
	)
	if !shutdown(context.Background(), slog.New(slog.DiscardHandler), p) {
		t.Fatal("a shutdown with no stragglers was not clean")
	}
	at := func(name string) int { return slices.Index(r.order, name) }
	seq := []int{
		at("intake"), at("cancel"), max(at("wait:discord"), at("wait:incident")),
		at("sub:discord"), at("sub:journal"), at("db"),
	}
	if slices.Contains(seq, -1) || !slices.IsSorted(seq) ||
		min(at("wait:discord"), at("wait:incident")) < at("cancel") {
		t.Errorf("shutdown order = %v, want intake < cancel < waits < subscriptions < db", r.order)
	}
}

func TestShutdownBound_StopsWaitingAtDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var r recorder
		release := make(chan struct{})
		p := r.plan([]step{{name: "sweep", run: within(func() { <-release })}}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		began := time.Now()
		clean := shutdown(ctx, slog.New(slog.DiscardHandler), p)
		if took := time.Since(began); took != shutdownBudget {
			t.Errorf("shutdown took %v, want the %v budget", took, shutdownBudget)
		}
		if clean || r.has("db") {
			t.Errorf("clean = %v, order %v: a straggler must skip the db close", clean, r.order)
		}
		close(release)
	})
}

func TestShutdown_HTTPDeadlineCountsAsStraggler(t *testing.T) {
	t.Parallel()
	var r recorder
	var logs bytes.Buffer
	p := r.plan(nil, nil)
	p.http = fmt.Errorf("%w: %w", server.ErrDrainTimeout, context.DeadlineExceeded)
	if shutdown(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), p) {
		t.Error("an HTTP drain that hit its deadline counted as clean")
	}
	if r.has("db") {
		t.Error("db closed with dashboard handlers still running")
	}
	if !strings.Contains(logs.String(), "stragglers=[http]") {
		t.Errorf("logs do not name http as the straggler:\n%s", logs.String())
	}
}

func TestShutdown_SkipsDBCloseWhenStragglersRemain(t *testing.T) {
	t.Parallel()
	var r recorder
	p := r.plan(nil, []step{r.step("sub:discord", context.DeadlineExceeded)})
	if shutdown(context.Background(), slog.New(slog.DiscardHandler), p) || r.has("db") {
		t.Errorf("order %v: a subscription that did not close must skip the db close", r.order)
	}
}

func TestShutdown_OnlyBoundHitLogsAboveInfo(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{"clean": nil, "straggler": errors.New("still running: run:abcd1234")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var r recorder
			var logs bytes.Buffer
			p := r.plan([]step{r.step("incident", err)}, []step{r.step("sub:journal", nil)})
			shutdown(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), p)
			var loud []string
			for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
				if !strings.Contains(line, "level=INFO") {
					loud = append(loud, line)
				}
			}
			if err == nil && len(loud) != 0 {
				t.Errorf("a clean shutdown logged above INFO: %q", loud)
			}
			if err != nil && (len(loud) != 1 || !strings.Contains(loud[0], `level=ERROR msg="shutdown bound hit"`)) {
				t.Errorf("lines above INFO = %q, want only the bound-hit line", loud)
			}
		})
	}
}

func TestMigrateDownFlag_ExitCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{fmt.Errorf("wrap: %w", db.ErrDownRefused), 2},
		{errors.New("boom"), 1},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
