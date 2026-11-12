package agent

import "sync"

type BazarrWaits struct {
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
}

func (w *BazarrWaits) Wait(key string) (<-chan struct{}, func()) {
	if w == nil {
		return nil, func() {}
	}
	ch := make(chan struct{})
	w.mu.Lock()
	if w.waiters == nil {
		w.waiters = map[string]map[chan struct{}]struct{}{}
	}
	if w.waiters[key] == nil {
		w.waiters[key] = map[chan struct{}]struct{}{}
	}
	w.waiters[key][ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.waiters[key], ch)
		if len(w.waiters[key]) == 0 {
			delete(w.waiters, key)
		}
	}
}

func (w *BazarrWaits) Notify(key string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.waiters[key] {
		close(ch)
	}
	delete(w.waiters, key)
}
