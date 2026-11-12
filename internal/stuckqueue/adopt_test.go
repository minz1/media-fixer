package stuckqueue_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func queuedDownload(t *testing.T, e *runEnv, app stuckqueue.App, downloadID string) stuckqueue.Download {
	t.Helper()
	f := e.sonarr
	if app == stuckqueue.AppRadarr {
		f = e.radarr
	}
	entries, err := f.client().QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range stuckqueue.Group(app, entries) {
		if d.DownloadID == downloadID {
			return d
		}
	}
	t.Fatalf("%s is not in the queue", downloadID)
	return stuckqueue.Download{}
}

func createdOp(t *testing.T, e *runEnv, spec operation.Spec) operation.Op {
	t.Helper()
	op, err := e.db.ApplyOperation(context.Background(), 0, func(operation.Op) (operation.Op, error) {
		return operation.Next(operation.Op{}, operation.Event{Kind: operation.Created, At: e.clock.Now(), Spec: &spec})
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func crashedOp(t *testing.T, e *runEnv, downloadID string) operation.Op {
	t.Helper()
	ctx := context.Background()
	op := createdOp(t, e, stuckqueue.RegrabSpecForTest(queuedDownload(t, e, stuckqueue.AppSonarr, downloadID)))
	_, err := e.db.ApplyOperation(ctx, op.ID, func(cur operation.Op) (operation.Op, error) {
		return operation.Next(cur, operation.Event{Kind: operation.StepStarted, At: e.clock.Now(), Index: 0})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.db.InterruptRunningOperations(ctx, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := e.db.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !operation.Resumable(got) {
		t.Fatalf("setup: op = %+v is not resumable", got)
	}
	return got
}

func adoptNow(t *testing.T, s *stuckqueue.Scheduler) {
	t.Helper()
	if err := s.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func adoptedMsg(adopted, caught int) string {
	return fmt.Sprintf("stuck-queue: adopted %d re-grab ops, caught up %d", adopted, caught)
}

func logLineWith(e *runEnv, from int, substr string) string {
	for line := range strings.SplitSeq(e.logs.String()[from:], "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

func seedRow(t *testing.T, e *runEnv, snap stuckqueue.Snapshot) {
	t.Helper()
	err := e.db.ApplyStuck(context.Background(), string(snap.Key.App), []string{snap.Key.DownloadID},
		func(map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
			return []db.StuckRow{stuckqueue.ToRowForTest(snap)}, nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

func TestQ2_8_CrashAfterRemovalAdoptsOp(t *testing.T) {
	t.Parallel()
	for name, gone := range map[string]bool{"download gone": true, "still stuck past the grace": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, s := regrabEnv(t)
			op := crashedOp(t, e, "D")
			if _, ok := e.rowOf(t, "D"); ok {
				t.Fatal("setup: a row exists before the adoption")
			}
			adoptNow(t, s)
			check := func(stage string) {
				t.Helper()
				row, ok := e.rowOf(t, "D")
				unfinished := row.ActedAction == string(stuckqueue.ActionRegrab) &&
					row.ActedResult == string(stuckqueue.ResultUnfinished) && row.ActedOpID == op.ID
				if !ok || row.State != string(stuckqueue.StateActed) || !unfinished {
					t.Fatalf("%s: row = %+v, %v; want acted regrab unfinished op %d", stage, row, ok, op.ID)
				}
				up, rendered := render(t, s, stuckSub())
				if !rendered {
					t.Fatalf("%s: the card is gone", stage)
				}
				resumeButton(t, up)
			}
			check("after the adoption")
			if gone {
				e.sonarr.set()
			}
			e.runner.Run(context.Background(), liveSettings())
			e.clock.Add(stuckqueue.Grace + time.Minute)
			e.runner.Run(context.Background(), liveSettings())
			e.clock.Add(stuckqueue.Grace + time.Minute)
			e.runner.Run(context.Background(), liveSettings())
			check("after the runs")
		})
	}
}

func TestEvAdopted_ExpiredOpNotReadopted(t *testing.T) {
	t.Parallel()
	t.Run("op older than Keep", func(t *testing.T) {
		t.Parallel()
		e, s := regrabEnv(t)
		crashedOp(t, e, "D")
		e.clock.Add(stuckqueue.Keep + time.Hour)
		adoptNow(t, s)
		if _, ok := e.rowOf(t, "D"); ok {
			t.Fatal("an op past Keep was adopted")
		}
		if logLineWith(e, 0, adoptedMsg(0, 0)) == "" {
			t.Fatalf("logs = %s", e.logs.String())
		}
	})
	t.Run("row expired after its last Resume", func(t *testing.T) {
		t.Parallel()
		e, s, _ := failSearch(t)
		e.sonarr.set()
		e.clock.Add(stuckqueue.Keep + time.Hour)
		s.Tick(e.clock.Now())
		s.Wait()
		if _, ok := e.rowOf(t, "D"); ok {
			t.Fatal("setup: the row did not expire")
		}
		adoptNow(t, s)
		if _, ok := e.rowOf(t, "D"); ok {
			t.Fatal("an expired row was adopted back")
		}
	})
}

func TestEvAdopted_MatchesKeyCaseInsensitively(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*runEnv, *stuckqueue.Scheduler, operation.Op) {
		t.Helper()
		e, s := newSched(t)
		e.sonarr.set(rawRec(1, "ABC123", "NOVA", 1, false, sample))
		op := crashedOp(t, e, "ABC123")
		if !strings.HasSuffix(op.Subject.ID, ":abc123") || op.Steps[0].DownloadID != "ABC123" {
			t.Fatalf("setup: op = %+v, want a lowercased subject and an original-case step", op)
		}
		return e, s, op
	}
	t.Run("an existing row of another case", func(t *testing.T) {
		t.Parallel()
		e, s, op := setup(t)
		if err := seedSeen(e, stuckqueue.AppSonarr, "Abc123", e.clock.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		adoptNow(t, s)
		rows, err := e.db.ListStuck(context.Background(), "sonarr")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].DownloadID != "Abc123" || rows[0].ActedOpID != op.ID ||
			rows[0].ActedResult != string(stuckqueue.ResultUnfinished) {
			t.Fatalf("rows = %+v, want the one stored row adopted", rows)
		}
	})
	t.Run("no row", func(t *testing.T) {
		t.Parallel()
		e, s, op := setup(t)
		adoptNow(t, s)
		rows, err := e.db.ListStuck(context.Background(), "sonarr")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].DownloadID != "ABC123" || rows[0].ActedOpID != op.ID {
			t.Fatalf("rows = %+v, want one new row in the recorded case", rows)
		}
	})
}

func TestEvAdopted_SecondStartIsSilent(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	crashedOp(t, e, "D")
	adoptNow(t, s)
	line := logLineWith(e, 0, adoptedMsg(1, 0))
	if !strings.Contains(line, "level=WARN") {
		t.Fatalf("first start: line = %q, want a WARN", line)
	}
	first, _ := e.rowOf(t, "D")
	from := len(e.logs.String())
	adoptNow(t, s)
	second, _ := e.rowOf(t, "D")
	if fmt.Sprintf("%+v", first) != fmt.Sprintf("%+v", second) {
		t.Fatalf("the second start wrote: %+v -> %+v", first, second)
	}
	line = logLineWith(e, from, adoptedMsg(0, 0))
	if line == "" || strings.Contains(e.logs.String()[from:], "level=WARN") {
		t.Fatalf("second start logs = %q", e.logs.String()[from:])
	}
}

func TestEvAdopted_ImportInflightWarnsAndSkips(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	crashedOp(t, e, "D")
	now := e.clock.Now()
	seedRow(t, e, stuckqueue.Snapshot{
		Key: sonarrKey("D"), State: stuckqueue.StateImportInflight, FirstSeen: now.Add(-time.Hour),
		Import: &stuckqueue.ImportCmd{CommandID: 5, StartedAt: now, Files: stuckqueue.Files{N: 1, M: 1}},
	})
	before, _ := e.rowOf(t, "D")
	adoptNow(t, s)
	after, _ := e.rowOf(t, "D")
	if fmt.Sprintf("%+v", before) != fmt.Sprintf("%+v", after) ||
		after.State != string(stuckqueue.StateImportInflight) {
		t.Fatalf("row %+v -> %+v, want it untouched", before, after)
	}
	warned := false
	for line := range strings.SplitSeq(e.logs.String(), "\n") {
		warned = warned || (strings.Contains(line, "level=WARN") && strings.Contains(line, "import"))
	}
	if !warned || logLineWith(e, 0, adoptedMsg(0, 0)) == "" {
		t.Fatalf("logs = %s", e.logs.String())
	}
}

func opsDump(t *testing.T, e *runEnv) string {
	t.Helper()
	raw, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.QueryContext(
		context.Background(),
		`SELECT id, subject_kind, subject_id, kind, title, source, steps, results, step_index, status, cause,
		version, created_at, updated_at FROM operations ORDER BY id`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "%q\n", fmt.Sprint(vals...))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestStuckQueue_NeverMovesAnOp(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(
		rawRec(1, "D1", "ONE", 1, false, sample), rawRec(2, "D2", "TWO", 2, false, sample),
		rawRec(3, "D3", "THREE", 3, false, sample), rawRec(4, "D4", "FOUR", 4, false, sample),
		rawRec(5, "D5", "FIVE", 5, false, sample),
	)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D1", stuckqueue.ActionRegrab); err != nil {
		t.Fatal(err)
	}
	e.sonarr.mu.Lock()
	e.sonarr.failCommand["EpisodeSearch"] = true
	e.sonarr.mu.Unlock()
	if _, err := act(e, s, stuckqueue.AppSonarr, "D2", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the search failure was not returned")
	}
	e.sonarr.mu.Lock()
	e.sonarr.failCommand["EpisodeSearch"] = false
	e.sonarr.mu.Unlock()
	e.sonarr.failDelete(400, false)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D3", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the removal failure was not returned")
	}
	e.sonarr.failDelete(0, false)
	crashedOp(t, e, "D4")
	createdOp(t, e, stuckqueue.RegrabSpecForTest(queuedDownload(t, e, stuckqueue.AppSonarr, "D5")))
	before := opsDump(t, e)
	for _, want := range []string{"done", "failed_at_step", "running"} {
		if !strings.Contains(before, want) {
			t.Fatalf("setup: no %s op in %s", want, before)
		}
	}
	e.clock.Add(7 * time.Hour)
	s.Tick(e.clock.Now())
	s.Wait()
	e.clock.Add(2 * time.Hour)
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	e.clock.Add(2 * time.Hour)
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	adoptNow(t, s)
	for _, id := range []string{"D1", "D2", "D3", "D4", "D5"} {
		_, _ = s.CardRenderer()(context.Background(), stuckSubFor(id))
	}
	if after := opsDump(t, e); after != before {
		t.Fatalf("an op changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func stuckSubFor(downloadID string) string { return card.StuckSubject("s", downloadID) }

func TestL10_UnknownAppOpIsWarnedOnce(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	ctx := context.Background()
	spec := stuckqueue.RegrabSpecForTest(queuedDownload(t, e, stuckqueue.AppSonarr, "D"))
	spec.Subject.ID = "s:x"
	for i := range spec.Steps {
		spec.Steps[i].DownloadID = "X"
	}
	odd := createdOp(t, e, spec)
	if _, err := e.db.ApplyOperation(ctx, odd.ID, func(cur operation.Op) (operation.Op, error) {
		return operation.Next(cur, operation.Event{Kind: operation.StepStarted, At: e.clock.Now(), Index: 0})
	}); err != nil {
		t.Fatal(err)
	}
	op := crashedOp(t, e, "D")
	e.rawExec(t, `UPDATE operations SET steps = replace(steps, '"sonarr"', '"lidarr"') WHERE subject_id = 's:x'`)
	from := len(e.logs.String())
	adoptNow(t, s)

	if n := strings.Count(e.logs.String()[from:], "names an unknown app"); n != 1 {
		t.Fatalf("unknown-app WARNs = %d, want 1:\n%s", n, e.logs.String()[from:])
	}
	if line := logLineWith(e, from, "names an unknown app"); !strings.Contains(line, "level=WARN") ||
		!strings.Contains(line, "app=lidarr") {
		t.Fatalf("line = %q", line)
	}
	if rows, err := e.db.ListStuck(ctx, "lidarr"); err != nil || len(rows) != 0 {
		t.Fatalf("lidarr rows = %+v (err %v), want none", rows, err)
	}
	if row, ok := e.rowOf(t, "D"); !ok || row.ActedOpID != op.ID {
		t.Fatalf("row = %+v, %v; want D adopted with op %d", row, ok, op.ID)
	}
}
