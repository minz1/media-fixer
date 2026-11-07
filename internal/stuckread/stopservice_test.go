package stuckread_test

import (
	"context"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/incident"
)

func stopService(t *testing.T, svc *incident.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Errorf("service shutdown: %v", err)
	}
}
