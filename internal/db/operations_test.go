package db_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
)

func opAt(n int) time.Time { return time.Date(2026, 11, 7, 12, n, 0, 0, time.UTC) }

func replaceSpecFor(id string) operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectIncident, ID: id}, Kind: operation.KindReplace,
		Title: "Show S01", Source: []byte(`{"plan":1}`),
		Steps: []operation.Step{
			{Kind: operation.StepDeleteFile, App: "sonarr", FileID: 3, Path: "/tv/e1.mkv", Size: 10, MediaID: 5},
			{Kind: operation.StepSearch, App: "sonarr", Command: "SeasonSearch", MediaID: 5, Season: 1},
		},
	}
}

func regrabSpecFor(id string) operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectDownload, ID: id}, Kind: operation.KindRegrab,
		Title: "Show S01E02",
		Steps: []operation.Step{
			{Kind: operation.StepQueueRemove, App: "sonarr", QueueID: 9, DownloadID: id, MediaID: 5},
			{Kind: operation.StepSearch, App: "sonarr", Command: "SeasonSearch", MediaID: 5, Season: 1},
		},
	}
}

func opDB(t *testing.T) *db.DB {
	t.Helper()
	return openDB(t, filepath.Join(t.TempDir(), "ops.db"))
}

func evolve(ctx context.Context, d *db.DB, id int64, ev operation.Event) (operation.Op, error) {
	return d.ApplyOperation(ctx, id, func(cur operation.Op) (operation.Op, error) { return operation.Next(cur, ev) })
}

func step(t *testing.T, d *db.DB, id int64, evs ...operation.Event) operation.Op {
	t.Helper()
	var op operation.Op
	for _, ev := range evs {
		var err error
		if op, err = evolve(context.Background(), d, id, ev); err != nil {
			t.Fatalf("%+v: %v", ev, err)
		}
	}
	return op
}

func newOp(t *testing.T, d *db.DB, spec operation.Spec) operation.Op {
	t.Helper()
	return step(t, d, 0, operation.Event{Kind: operation.Created, At: opAt(0), Spec: &spec})
}

func startEv(i int) operation.Event {
	return operation.Event{Kind: operation.StepStarted, At: opAt(1), Index: i}
}

func finishEv(i int, s operation.StepState) operation.Event {
	return operation.Event{Kind: operation.StepFinished, At: opAt(2), Index: i, Outcome: s, Err: "boom"}
}

func finishDone(t *testing.T, d *db.DB, id int64) {
	t.Helper()
	step(t, d, id, startEv(0), finishEv(0, operation.ResultDone), startEv(1), finishEv(1, operation.ResultDone))
}

type opRow struct {
	subjectKind, subjectID, kind, source, steps, results, status, cause string
	index                                                               int
}

func twoJSON() string { return `[{},{}]` }

func goodRow() opRow {
	return opRow{"incident", "i1", "replace", "{}", twoJSON(), twoJSON(), "running", "", 0}
}

func putRow(raw execer, r opRow) error {
	_, err := raw.ExecContext(context.Background(), `INSERT INTO operations (subject_kind, subject_id, kind, source,
		steps, results, step_index, status, cause, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.subjectKind, r.subjectID, r.kind, r.source, r.steps, r.results,
		r.index, r.status, r.cause, opAt(0), opAt(0))
	return err
}

func TestMigration22_CreatesTableWithChecks(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "checks.db")
	if err := db.MigrateUpToForTest(path, 22); err != nil {
		t.Fatal(err)
	}
	conn := rawOpen(t, path)
	if err := putRow(conn, goodRow()); err != nil {
		t.Fatalf("good row: %v", err)
	}
	mustExec(t, conn, `DELETE FROM operations`)
	long := "[" + strings.Repeat("{},", 2000) + "{}]"
	bad := map[string]func(*opRow){
		"status":        func(r *opRow) { r.status = "paused" },
		"cause":         func(r *opRow) { r.status, r.cause = "failed_at_step", "boom" },
		"cause missing": func(r *opRow) { r.status = "failed_at_step" },
		"cause on run":  func(r *opRow) { r.cause = "error" },
		"results len":   func(r *opRow) { r.results = `[{}]` },
		"index range":   func(r *opRow) { r.index = 2 },
		"done not last": func(r *opRow) { r.status = "done" },
		"regrab on inc": func(r *opRow) { r.kind = "regrab" },
		"incident dl":   func(r *opRow) { r.subjectKind = "download" },
		"too many":      func(r *opRow) { r.steps, r.results = long, long },
		"bad source":    func(r *opRow) { r.source = "{" },
		"steps object":  func(r *opRow) { r.steps = `{}` },
		"empty subject": func(r *opRow) { r.subjectID = "" },
	}
	for name, mod := range bad {
		r := goodRow()
		mod(&r)
		if err := putRow(conn, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	checkRunningIndex(t, conn)
}

func checkRunningIndex(t *testing.T, conn execer) {
	t.Helper()
	if err := putRow(conn, goodRow()); err != nil {
		t.Fatal(err)
	}
	if err := putRow(conn, goodRow()); err == nil {
		t.Error("a second running op for one subject was accepted")
	}
	done := goodRow()
	done.status, done.index = "done", 1
	for range 2 {
		if err := putRow(conn, done); err != nil {
			t.Errorf("done op refused: %v", err)
		}
	}
}

func TestMigration22_PreviousBinaryIgnoresIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "v22.db")
	d := openDB(t, path)
	spec := replaceSpecFor("inc-1")
	op := newOp(t, d, spec)
	versions, err := d.SchemaVersionsForTest(context.Background())
	if err != nil || versions[len(versions)-1] != 22 {
		t.Fatalf("versions %v %v", versions, err)
	}
	d.Close()
	ran, err := db.MigrateTruncatedForTest(path, 21)
	if err != nil || len(ran) != 0 {
		t.Fatalf("truncated list ran %v: %v", ran, err)
	}
	raw := rawOpen(t, path)
	for _, q := range []string{
		`SELECT count(*) FROM incidents`, `SELECT count(*) FROM stuck_queue_downloads`,
		`SELECT count(*) FROM incident_events`,
	} {
		var n int
		if err = raw.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	var n int
	mustScan(t, raw, `SELECT count(*) FROM operations WHERE id = ?`, &n, op.ID)
	if n != 1 {
		t.Errorf("op row count %d", n)
	}
}

func mustScan(t *testing.T, raw interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, q string, dst *int, args ...any) {
	t.Helper()
	if err := raw.QueryRowContext(context.Background(), q, args...).Scan(dst); err != nil {
		t.Fatal(err)
	}
}

func TestMigration22_SyntheticSnapshotShape(t *testing.T) {
	t.Parallel()
	path, raw := schema20(t)
	insertAll(t, raw, syntheticRows())
	if _, err := db.MigrateTruncatedForTest(path, 21); err != nil {
		t.Fatal(err)
	}
	before := tableCounts(t, raw)
	openDB(t, path)
	after := tableCounts(t, raw)
	if after["operations"] != 0 {
		t.Errorf("operations has %d rows", after["operations"])
	}
	delete(after, "operations")
	delete(after, "schema_version")
	delete(before, "schema_version")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("counts changed: %v -> %v", before, after)
	}
}

func tableCounts(t *testing.T, raw interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) map[string]int {
	t.Helper()
	rows, err := raw.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, n := range names {
		var c int
		mustScan(t, raw, `SELECT count(*) FROM "`+n+`"`, &c)
		out[n] = c
	}
	return out
}

func TestApplyOperation_ChecksBeforeWrite(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	ctx := context.Background()
	op := newOp(t, d, replaceSpecFor("inc-1"))
	before, err := d.Operation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	illegal := func(cur operation.Op) (operation.Op, error) {
		next := cur
		next.Version++
		next.Results = []operation.Result{{State: "bogus"}, {State: operation.ResultPending}}
		return next, nil
	}
	if _, err = d.ApplyOperation(ctx, op.ID, illegal); !errors.Is(err, operation.ErrIllegal) {
		t.Fatalf("err %v, want ErrIllegal", err)
	}
	after, _ := d.Operation(ctx, op.ID)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("illegal op was written: %+v", after)
	}
	_, err = d.ApplyOperation(ctx, 0, func(operation.Op) (operation.Op, error) {
		return operation.Op{Status: operation.Running}, nil
	})
	if !errors.Is(err, operation.ErrIllegal) {
		t.Errorf("illegal create: %v", err)
	}
	none := operation.Subject{Kind: operation.SubjectIncident, ID: ""}
	if _, err = d.LatestOperation(ctx, none); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("illegal create left a row: %v", err)
	}
}

func TestApplyOperation_VersionBumpsOnEveryAccept(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	ctx := context.Background()
	op := newOp(t, d, replaceSpecFor("inc-1"))
	if op.ID == 0 || op.Version != 0 {
		t.Fatalf("created %d v%d", op.ID, op.Version)
	}
	evs := []operation.Event{
		startEv(0), finishEv(0, operation.ResultDone), startEv(1), finishEv(1, operation.ResultDone),
	}
	for i, ev := range evs {
		got := step(t, d, op.ID, ev)
		stored, err := d.Operation(ctx, op.ID)
		if err != nil || stored.Version != int64(i+1) || got.Version != stored.Version {
			t.Fatalf("event %d: stored v%d returned v%d: %v", i, stored.Version, got.Version, err)
		}
	}
	noBump := func(cur operation.Op) (operation.Op, error) { return cur, nil }
	if _, err := d.ApplyOperation(ctx, op.ID, noBump); err == nil {
		t.Error("an unchanged version was accepted")
	}
}

func TestApplyOperation_RejectWritesNothing(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	ctx := context.Background()
	op := newOp(t, d, replaceSpecFor("inc-1"))
	before, _ := d.Operation(ctx, op.ID)
	_, err := evolve(ctx, d, op.ID, startEv(1))
	if operation.CodeOf(err) != operation.CodeOutOfOrder {
		t.Fatalf("err %v", err)
	}
	boom := errors.New("decide failed")
	fails := func(operation.Op) (operation.Op, error) { return operation.Op{}, boom }
	if _, err = d.ApplyOperation(ctx, op.ID, fails); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	after, _ := d.Operation(ctx, op.ID)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("a refusal wrote: %+v", after)
	}
	if _, err = evolve(ctx, d, 999, startEv(0)); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("missing op: %v", err)
	}
}

func TestInterruptRunningOperations_OnlyTouchesRunning(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	ctx := context.Background()
	run := newOp(t, d, replaceSpecFor("inc-run"))
	step(t, d, run.ID, startEv(0))
	idle := newOp(t, d, regrabSpecFor("dl-idle"))
	done := newOp(t, d, replaceSpecFor("inc-done"))
	finishDone(t, d, done.ID)
	failed := newOp(t, d, replaceSpecFor("inc-failed"))
	step(t, d, failed.ID, startEv(0), finishEv(0, operation.ResultFailed))
	doneBefore, _ := d.Operation(ctx, done.ID)
	failedBefore, _ := d.Operation(ctx, failed.ID)
	got, err := d.InterruptRunningOperations(ctx, opAt(9))
	if err != nil || len(got) != 2 {
		t.Fatalf("interrupted %d: %v", len(got), err)
	}
	for _, op := range got {
		stored, _ := d.Operation(ctx, op.ID)
		if stored.Status != operation.FailedAtStep || stored.Cause != operation.CauseRestart ||
			!stored.UpdatedAt.Equal(opAt(9)) {
			t.Errorf("%d: %+v", op.ID, stored)
		}
	}
	if s, _ := d.Operation(ctx, run.ID); s.Results[0].State != operation.ResultUnknown {
		t.Errorf("started step is %s", s.Results[0].State)
	}
	if s, _ := d.Operation(ctx, idle.ID); s.Results[0].State != operation.ResultPending {
		t.Errorf("pending step is %s", s.Results[0].State)
	}
	doneAfter, _ := d.Operation(ctx, done.ID)
	failedAfter, _ := d.Operation(ctx, failed.ID)
	if !reflect.DeepEqual(doneBefore, doneAfter) || !reflect.DeepEqual(failedBefore, failedAfter) {
		t.Error("a non-running op changed")
	}
	if again, _ := d.InterruptRunningOperations(ctx, opAt(10)); len(again) != 0 {
		t.Errorf("second pass interrupted %d", len(again))
	}
}

func TestLatestOperation_BySubject(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	ctx := context.Background()
	first := newOp(t, d, replaceSpecFor("inc-1"))
	finishDone(t, d, first.ID)
	second := newOp(t, d, replaceSpecFor("inc-1"))
	other := newOp(t, d, replaceSpecFor("inc-2"))
	got, err := d.LatestOperation(ctx, operation.Subject{Kind: operation.SubjectIncident, ID: "inc-1"})
	if err != nil || got.ID != second.ID {
		t.Fatalf("latest %d, want %d: %v", got.ID, second.ID, err)
	}
	inc2 := operation.Subject{Kind: operation.SubjectIncident, ID: "inc-2"}
	if got, _ = d.LatestOperation(ctx, inc2); got.ID != other.ID {
		t.Errorf("inc-2 latest %d", got.ID)
	}
	_, err = d.LatestOperation(ctx, operation.Subject{Kind: operation.SubjectDownload, ID: "inc-1"})
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("other kind: %v", err)
	}
}

func TestResumableDownloadOperations_LatestResumableOnly(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	ctx := context.Background()
	oldFail := newOp(t, d, regrabSpecFor("dl-1"))
	step(t, d, oldFail.ID, startEv(0), finishEv(0, operation.ResultUnknown))
	newDone := newOp(t, d, regrabSpecFor("dl-1"))
	finishDone(t, d, newDone.ID)
	oldDone := newOp(t, d, regrabSpecFor("dl-2"))
	finishDone(t, d, oldDone.ID)
	want := newOp(t, d, regrabSpecFor("dl-2"))
	step(t, d, want.ID, startEv(0), finishEv(0, operation.ResultUnknown))
	notApplied := newOp(t, d, regrabSpecFor("dl-3"))
	step(t, d, notApplied.ID, startEv(0), finishEv(0, operation.ResultFailed))
	newOp(t, d, regrabSpecFor("dl-4"))
	inc := newOp(t, d, replaceSpecFor("inc-1"))
	step(t, d, inc.ID, startEv(0), finishEv(0, operation.ResultUnknown))
	got, err := d.ResumableDownloadOperations(ctx)
	if err != nil || len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("got %+v: %v", got, err)
	}
}

func TestOperationWriter_NeverUpdatesStepsOrSource(t *testing.T) {
	t.Parallel()
	d := opDB(t)
	ctx := context.Background()
	op := newOp(t, d, replaceSpecFor("inc-1"))
	err := d.ExecForTest(ctx, `CREATE TRIGGER no_steps_or_source BEFORE UPDATE OF steps, source ON operations
		BEGIN SELECT RAISE(ABORT, 'steps and source are immutable'); END`)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := d.Operation(ctx, op.ID)
	_, err = d.ApplyOperation(ctx, op.ID, func(cur operation.Op) (operation.Op, error) {
		next, nextErr := operation.Next(cur, startEv(0))
		next.Steps = append([]operation.Step(nil), next.Steps...)
		next.Steps[0].Path = "/tv/changed.mkv"
		next.Source = []byte(`{"plan":2}`)
		return next, nextErr
	})
	if err != nil {
		t.Fatalf("update touched steps or source: %v", err)
	}
	after, _ := d.Operation(ctx, op.ID)
	if !reflect.DeepEqual(before.Steps, after.Steps) || string(before.Source) != string(after.Source) {
		t.Errorf("steps or source changed: %+v %s", after.Steps, after.Source)
	}
	if after.Results[0].State != operation.ResultStarted {
		t.Errorf("results not written: %+v", after.Results)
	}
}
