package incident_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
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

func newRunningIncident(t *testing.T, logs *syncBuffer) (*incident.Service, *db.DB, string, *blockingAgent, string) {
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
	svc := incident.NewService(t.Context(), database, nil, ag, nil, nil, newSyncNotifier(), logger)

	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", ReportedBy: "x", What: "cant_play", Title: "Slow"}
	if err = database.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	if err = svc.Rerun(context.Background(), inc.ID); err != nil {
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
	svc, database, _, ag, id := newRunningIncident(t, &syncBuffer{})

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rerun(context.Background(), id); err == nil {
		t.Fatal("Rerun on a closed DB returned nil, want an error")
	}

	select {
	case <-ag.done:
		t.Fatal("a rerun that failed to read the incident cancelled the running run")
	case <-time.After(quietWindow):
	}
}

func TestRerun_StatusWriteFailureAfterCancelLogsError(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	svc, _, dbPath, ag, id := newRunningIncident(t, logs)

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(context.Background(), `CREATE TRIGGER refuse_status BEFORE UPDATE OF status
		ON incidents BEGIN SELECT RAISE(ABORT, 'status write refused'); END`); err != nil {
		t.Fatal(err)
	}

	if err = svc.Rerun(context.Background(), id); err == nil {
		t.Fatal("Rerun with a refused status write returned nil, want an error")
	}
	select {
	case <-ag.done:
	case <-time.After(notifyWaitTimeout):
		t.Fatal("timed out waiting for the old run to be cancelled")
	}

	var found bool
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, id) &&
			strings.Contains(line, "no replacement") {
			found = true
		}
	}
	if !found {
		t.Errorf("no ERROR log naming the incident and the lost run; logs:\n%s", logs.String())
	}
}
