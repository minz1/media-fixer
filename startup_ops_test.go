package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
)

func runningOps(t *testing.T, ids ...string) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	for _, id := range ids {
		spec := operation.Spec{
			Subject: operation.Subject{Kind: operation.SubjectIncident, ID: id}, Kind: operation.KindReplace,
			Title: "Show S01", Source: []byte(`{}`),
			Steps: []operation.Step{
				{Kind: operation.StepDeleteFile, App: "sonarr", FileID: 3, Path: "/tv/e1.mkv", Size: 10, MediaID: 5},
				{Kind: operation.StepSearch, App: "sonarr", Command: "SeasonSearch", MediaID: 5, Season: 1},
			},
		}
		_, err = database.ApplyOperation(context.Background(), 0, func(operation.Op) (operation.Op, error) {
			return operation.Next(operation.Op{}, operation.Event{Kind: operation.Created, At: time.Now(), Spec: &spec})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestStartup_InterruptRunsBeforeIntake(t *testing.T) {
	t.Parallel()
	var r recorder
	err := startup(startSteps{
		interrupt:   func() error { r.note("interrupt"); return nil },
		maintenance: r.fn("maintenance"), cards: r.fn("cards"), bot: r.fn("bot"), intake: r.fn("intake"),
		service: r.fn("service"), recovery: r.fn("recovery"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.order) == 0 || r.order[0] != "interrupt" {
		t.Errorf("startup order = %v, want interrupt first", r.order)
	}
}

func TestStartup_InterruptFailureStartsNothing(t *testing.T) {
	t.Parallel()
	var r recorder
	boom := errors.New("boom")
	err := startup(startSteps{
		interrupt:   func() error { return boom },
		maintenance: r.fn("maintenance"), cards: r.fn("cards"), bot: r.fn("bot"), intake: r.fn("intake"),
		service: r.fn("service"), recovery: r.fn("recovery"),
	})
	if !errors.Is(err, boom) || len(r.order) != 0 {
		t.Errorf("err = %v, started %v, want the error and nothing started", err, r.order)
	}
}

func TestStartup_InterruptLogsEachOpID(t *testing.T) {
	t.Parallel()
	database := runningOps(t, "inc-a", "inc-b")
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	if err := interruptOperations(context.Background(), database, log); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	for _, want := range []string{
		"level=WARN msg=\"operations: interrupted 2 at startup\"",
		"operations: interrupted op=1 subject=incident:inc-a step=0",
		"operations: interrupted op=2 subject=incident:inc-b step=0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
}

func TestStartup_InterruptNothingLogsInfo(t *testing.T) {
	t.Parallel()
	database := runningOps(t)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	if err := interruptOperations(context.Background(), database, log); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(buf.String(), "level=INFO msg=\"operations: interrupted 0 at startup\"") {
		t.Errorf("log = %s, want an INFO line for zero ops", buf.String())
	}
}
