package stuckread

import "time"

func (w *Watcher) SetNowForTest(now func() time.Time) { w.now = now }

func (w *Watcher) TrackedTitlesForTest() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.lastStuck)
}

func (w *Watcher) WarnedForTest() int { return len(w.warned) }
