package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/minz1/mediafixer/internal/server"
)

const shutdownBudget = 60 * time.Second

type step struct {
	name string
	run  func(ctx context.Context) error
}

type startSteps struct {
	maintenance func()
	cards       func()
	bot         func()
	intake      func()
	service     func()
	recovery    func()
}

func startup(st startSteps) {
	st.maintenance()
	st.cards()
	st.bot()
	st.intake()
	st.service()
	st.recovery()
}

type shutdownPlan struct {
	intake  func()
	cancel  func()
	http    error
	waits   []step
	subs    []step
	closeDB func() error
}

func shutdown(ctx context.Context, log *slog.Logger, p shutdownPlan) bool {
	began := time.Now()
	_ = runStep(ctx, log, step{name: "intake", run: plain(p.intake)})
	_ = runStep(ctx, log, step{name: "cancel", run: plain(p.cancel)})
	stragglers := waitAll(ctx, log, p.waits)
	if errors.Is(p.http, server.ErrDrainTimeout) {
		stragglers = append(stragglers, "http")
	}
	for _, st := range p.subs {
		if err := runStep(ctx, log, st); err != nil {
			stragglers = append(stragglers, st.name+": "+err.Error())
		}
	}
	clean := len(stragglers) == 0
	if clean {
		closeDB := step{name: "db", run: func(context.Context) error { return p.closeDB() }}
		if err := runStep(ctx, log, closeDB); err != nil {
			log.ErrorContext(ctx, "close database", "error", err)
		}
	} else {
		log.ErrorContext(ctx, "shutdown bound hit", "stragglers", stragglers)
	}
	log.InfoContext(ctx, "shutdown done", "total", time.Since(began), "clean", clean)
	return clean
}

func waitAll(ctx context.Context, log *slog.Logger, steps []step) []string {
	var mu sync.Mutex
	var stragglers []string
	var wg sync.WaitGroup
	for _, st := range steps {
		wg.Go(func() {
			if err := runStep(ctx, log, st); err != nil {
				mu.Lock()
				stragglers = append(stragglers, st.name+": "+err.Error())
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return stragglers
}

func runStep(ctx context.Context, log *slog.Logger, st step) error {
	began := time.Now()
	err := st.run(ctx)
	attrs := []any{"step", st.name, "duration", time.Since(began)}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	log.InfoContext(ctx, "shutdown step", attrs...)
	return err
}

func plain(fn func()) func(context.Context) error {
	return func(context.Context) error {
		fn()
		return nil
	}
}

func within(fn func()) func(context.Context) error {
	return func(ctx context.Context) error {
		done := make(chan struct{})
		go func() {
			fn()
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
