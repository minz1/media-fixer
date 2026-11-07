package incident_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

const quietWindow = 300 * time.Millisecond

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newRunningIncident(
	t *testing.T, logs *syncBuffer, notif *syncNotifier,
) (*incident.Service, *db.DB, string, *blockingAgent, string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	ag := newBlockingAgent()
	logger := slog.New(slog.NewTextHandler(logs, nil))
	svc := incident.NewService(t.Context(), database, nil, ag, nil, nil, notif, logger)
	t.Cleanup(func() { stopService(t, svc) })

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Slow"}
	if err = database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if err = svc.Rerun(context.Background(), inc.ID, incident.Version{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ag.started:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the first run to start")
	}
	return svc, database, f.Name(), ag, inc.ID
}

func TestRerun_ReadFailureLeavesTheRunningRunAlone(t *testing.T) {
	t.Parallel()
	svc, database, _, ag, id := newRunningIncident(t, &syncBuffer{}, newSyncNotifier())

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rerun(context.Background(), id, incident.Version{}); err == nil {
		t.Fatal("Rerun on a closed DB returned nil, want an error")
	}

	select {
	case <-ag.done:
		t.Fatal("a rerun that failed to read the incident cancelled the running run")
	case <-time.After(quietWindow):
	}
}
