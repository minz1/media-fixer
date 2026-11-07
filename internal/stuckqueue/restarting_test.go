package stuckqueue_test

import (
	"context"
	"errors"
	"testing"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func TestStuckQueue_ActAndImportRefusedWhenClosing(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	base, cancel := context.WithCancel(context.Background())
	cancel()
	s := stuckqueue.NewScheduler(base, e.runner, e.db)
	t.Cleanup(s.Wait)

	_, actErr := s.Act(context.Background(), stuckqueue.AppSonarr, "DL1", stuckqueue.ActionRemove, "fp")
	_, importErr := s.ImportMapped(context.Background(), 1, "DL1", "fp", bothRows())
	for name, err := range map[string]error{"Act": actErr, "ImportMapped": importErr} {
		if !errors.Is(err, card.ErrRestarting) {
			t.Errorf("%s while closing = %v, want ErrRestarting", name, err)
		}
	}
	for name, f := range map[string]*fakeArr{"sonarr": e.sonarr, "radarr": e.radarr} {
		f.mu.Lock()
		n := f.reqs
		f.mu.Unlock()
		if n != 0 {
			t.Errorf("%s got %d requests while closing, want none", name, n)
		}
	}
}
