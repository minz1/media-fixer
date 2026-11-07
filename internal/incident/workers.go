package incident

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
)

var ErrShutdownTimeout = errors.New("incident service shutdown timed out")

type workers struct {
	base   context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	seq    int
	live   map[int]string
}

func newWorkers(parent context.Context) *workers {
	base, cancel := context.WithCancel(parent)
	return &workers{base: base, cancel: cancel, live: make(map[int]string)}
}

func (w *workers) goNamed(name string, fn func(ctx context.Context)) bool {
	leave, ok := w.enter(name)
	if !ok {
		return false
	}
	go func() {
		defer leave()
		fn(w.base)
	}()
	return true
}

func (w *workers) enter(name string) (func(), bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, false
	}
	w.seq++
	id := w.seq
	w.live[id] = name
	w.wg.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.live, id)
			w.mu.Unlock()
			w.wg.Done()
		})
	}, true
}

func (w *workers) stop() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cancel()
}

func (w *workers) wait(ctx context.Context) []string {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(maps.Values(w.live))
}
