package agent_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

func TestRule8_DispatcherHoldsOnlyClients(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[agent.Dispatcher]()
	clientPkg := reflect.TypeFor[client.ArrClient]().PkgPath()
	for f := range typ.Fields() {
		if f.Type == reflect.TypeFor[*agent.BazarrWaits]() || f.Name == "Attempts" || f.Name == "ResyncMaxBytes" {
			continue
		}
		if f.Type.Kind() != reflect.Pointer || f.Type.Elem().PkgPath() != clientPkg {
			t.Errorf("Dispatcher.%s is %s, want a pointer to a type in %s", f.Name, f.Type, clientPkg)
		}
	}
}

func TestScope_ConcurrentScopesNoCrossTalk(t *testing.T) {
	t.Parallel()
	ma, hits := countingMediaAgent(t)
	disp := &agent.Dispatcher{MediaAgent: ma}
	args := map[string]any{"file_path": "/mnt/decypharr/x.mkv"}

	var wg sync.WaitGroup
	for range 50 {
		for _, tc := range []struct {
			what    string
			wantErr bool
		}{{agent.WhatStuckRead, true}, {"cant_play", false}} {
			wg.Go(func() {
				_, err := disp.CallScoped(context.Background(), agent.Scope{What: tc.what}, "dd_readability_test", args)
				if (err != nil) != tc.wantErr {
					t.Errorf("what %q: err = %v, wantErr %v", tc.what, err, tc.wantErr)
				}
			})
		}
	}
	wg.Wait()
	if got := hits.Load(); got != 50 {
		t.Errorf("media-agent saw %d requests, want 50", got)
	}
}
