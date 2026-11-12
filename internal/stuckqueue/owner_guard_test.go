package stuckqueue_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func seedSnap(t *testing.T, e *runEnv, s stuckqueue.Snapshot) {
	t.Helper()
	dl := s.Key.DownloadID
	err := e.db.ApplyStuck(context.Background(), string(s.Key.App), []string{dl},
		func(map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
			return []db.StuckRow{stuckqueue.ToRowForTest(s)}, nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

func waitingAt(e *runEnv, ago time.Duration) stuckqueue.Snapshot {
	return stuckqueue.Snapshot{
		Key: sonarrKey("D"), State: stuckqueue.StateWaiting, FirstSeen: e.clock.Now().Add(-ago),
		Offered: stuckqueue.OfferRemove,
	}
}

func inflightRow(e *runEnv, cmd int) stuckqueue.Snapshot {
	return stuckqueue.Snapshot{
		Key: sonarrKey("D"), State: stuckqueue.StateImportInflight, FirstSeen: e.clock.Now().Add(-8 * time.Hour),
		Import: &stuckqueue.ImportCmd{
			CommandID: cmd, StartedAt: e.clock.Now().Add(-time.Minute), Files: stuckqueue.Files{N: 2, M: 2},
		},
	}
}

func actedImportRow(e *runEnv, files stuckqueue.Files) stuckqueue.Snapshot {
	return stuckqueue.Snapshot{
		Key: sonarrKey("D"), State: stuckqueue.StateActed, FirstSeen: e.clock.Now().Add(-8 * time.Hour),
		Acted: &stuckqueue.Acted{
			Action: stuckqueue.ActionImport, At: e.clock.Now().Add(-time.Minute),
			Result: stuckqueue.ResultOK, Files: files,
		},
	}
}

func storedRow(t *testing.T, e *runEnv) db.StuckRow {
	t.Helper()
	row, ok := e.rowOf(t, "D")
	if !ok {
		t.Fatal("no stored row for D")
	}
	return row
}

func TestQ1_17_ImportMappedRefusedInsideWait(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	seedSnap(t, e, waitingAt(e, time.Hour))
	_, err := importMapped(e, s, t, bothRows())
	refusedNoWrite(t, e.sonarr, err, "inside the 6 h wait")
}

func TestQ1_17_ImportMappedRefusedWithoutRow(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	e.rawExec(t, "DELETE FROM stuck_downloads")
	_, err := importMapped(e, s, t, bothRows())
	refusedNoWrite(t, e.sonarr, err, "no run has checked this download yet")
}

func TestQ1_17_PastWallClockWaitActsAndRecords(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	seedSnap(t, e, waitingAt(e, 7*time.Hour))
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err != nil {
		t.Fatal(err)
	}
	row := storedRow(t, e)
	if row.State != string(stuckqueue.StateActed) || row.ActedAction != string(stuckqueue.ActionRegrab) ||
		row.ActedResult != string(stuckqueue.ResultOK) || row.ActedOpID <= 0 {
		t.Fatalf("row = %+v, want acted regrab ok with an op id", row)
	}
}

func TestQ1_18_SecondImportRefusedWhileCommandRuns(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	seedSnap(t, e, inflightRow(e, 41))
	e.sonarr.setCommandStatus(41, "started")
	_, err := importMapped(e, s, t, bothRows())
	refusedNoWrite(t, e.sonarr, err, "an import for this download is running")
	_, err = act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport)
	refusedNoWrite(t, e.sonarr, err, "an import for this download is running")
	if row := storedRow(t, e); row.State != string(stuckqueue.StateImportInflight) {
		t.Fatalf("row = %+v, want still import_inflight", row)
	}
}

func TestQ1_18_ActImportRefusedByPendingBackstop(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	e.sonarr.setCandidates("D", candidateJSON("D", 1, false))
	e.sonarr.setCommands(0, map[string]any{
		"name": "ManualImport", "status": "started",
		"body": map[string]any{"files": []map[string]any{{"path": "/data/downloads/sonarr/D/f.mkv"}}},
	})
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport)
	refusedNoWrite(t, e.sonarr, err, "an import for this download is already running")
}

func TestQ1_18_ImportMappedRecordsInflight(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	seedSnap(t, e, waitingAt(e, 7*time.Hour))
	if _, err := importMapped(e, s, t, bothRows()); err != nil {
		t.Fatal(err)
	}
	row := storedRow(t, e)
	if row.State != string(stuckqueue.StateImportInflight) || row.ImportCommandID <= 0 ||
		row.ImportFiles != 2 || row.ImportTotal != 2 {
		t.Fatalf("row = %+v, want import_inflight with the command and 2 of 2 files", row)
	}
}

func TestQ1_18_PartialMappedImportContinuesWithinGrace(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	seedSnap(t, e, actedImportRow(e, stuckqueue.Files{N: 1, M: 2}))
	rest := importRows(stuckqueue.MappedFile{Path: pathB, EpisodeIDs: []int{2}})
	if _, err := importMapped(e, s, t, rest); err != nil {
		t.Fatal(err)
	}
	row := storedRow(t, e)
	if row.State != string(stuckqueue.StateImportInflight) || row.ImportCommandID <= 0 {
		t.Fatalf("row = %+v, want import_inflight", row)
	}
}

func TestQ1_18_ImportButtonOnActedImportRefused(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	e.sonarr.setCandidates("D", candidateJSON("D", 1, false))
	seedSnap(t, e, actedImportRow(e, stuckqueue.Files{N: 1, M: 2}))
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport)
	refusedNoWrite(t, e.sonarr, err, "already imported")
}

func TestQ2_8_UnfinishedRegrabPersisted(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failCommand["EpisodeSearch"] = true
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err == nil {
		t.Fatal("setup: the search failure was not returned")
	}
	op := latestOp(t, e)
	row := storedRow(t, e)
	if row.State != string(stuckqueue.StateActed) || row.ActedAction != string(stuckqueue.ActionRegrab) ||
		row.ActedResult != string(stuckqueue.ResultUnfinished) || row.ActedOpID != op.ID {
		t.Fatalf("row = %+v, want acted regrab unfinished with op %d", row, op.ID)
	}
	s2 := stuckqueue.NewScheduler(context.Background(), e.runner, e.db)
	t.Cleanup(s2.Wait)
	if _, hasRun, running := s2.State(); hasRun || running {
		t.Fatal("a new scheduler claims a run it never made")
	}
	if got := storedRow(t, e); got.ActedResult != string(stuckqueue.ResultUnfinished) || got.ActedOpID != op.ID {
		t.Fatalf("row after restart = %+v", got)
	}
}

func TestQ5b_4_RemovalUnknownRecordsUnfinished(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusGatewayTimeout, true)
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab)
	if err == nil || strings.Contains(err.Error(), "search failed") {
		t.Fatalf("err = %v, want the removal failure and not a search failure", err)
	}
	row := storedRow(t, e)
	if row.State != string(stuckqueue.StateActed) || row.ActedAction != string(stuckqueue.ActionRegrab) ||
		row.ActedResult != string(stuckqueue.ResultUnfinished) {
		t.Fatalf("row = %+v, want acted regrab unfinished", row)
	}
}

func TestRegrab_DoneRecordsOkWithOpID(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab); err != nil {
		t.Fatal(err)
	}
	op := latestOp(t, e)
	row := storedRow(t, e)
	if op.Status != operation.Done || row.ActedResult != string(stuckqueue.ResultOK) || row.ActedOpID != op.ID {
		t.Fatalf("row = %+v, op = %+v, want acted ok with op %d", row, op, op.ID)
	}
}

func TestRegrab_DefinitiveRemovalFailureRecordsNothing(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	e.sonarr.failDelete(http.StatusBadRequest, false)
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab)
	if err == nil || errors.Is(err, stuckqueue.ErrRefused) {
		t.Fatalf("err = %v, want the removal failure", err)
	}
	if row := storedRow(t, e); row.State == string(stuckqueue.StateActed) || row.ActedAction != "" {
		t.Fatalf("row = %+v, want no acted record", row)
	}
}

func TestP6Followup_ImportCommandIDZeroOnActedImportRecordsOutcome(t *testing.T) {
	t.Parallel()
	e, s := importEnv(t)
	seedSnap(t, e, actedImportRow(e, stuckqueue.Files{N: 1, M: 2}))
	e.sonarr.mu.Lock()
	e.sonarr.nextID = -1
	e.sonarr.mu.Unlock()
	rest := importRows(stuckqueue.MappedFile{Path: pathB, EpisodeIDs: []int{2}})
	if _, err := importMapped(e, s, t, rest); err != nil {
		t.Fatal(err)
	}
	row := storedRow(t, e)
	if row.State != string(stuckqueue.StateActed) || row.ActedAction != string(stuckqueue.ActionImport) ||
		!row.ActedAt.Equal(e.clock.Now()) {
		t.Fatalf("row = %+v, want acted import recorded at %v", row, e.clock.Now())
	}
}

func TestL8_OwnerGuardIgnoresDownloadIDCase(t *testing.T) {
	t.Parallel()
	e, s := regrabEnv(t)
	fp := ""
	entries, err := e.sonarr.client().QueueEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range stuckqueue.Group(stuckqueue.AppSonarr, entries) {
		if d.DownloadID == "D" {
			fp = d.TargetsFP()
		}
	}
	lower := func() error {
		_, actErr := s.Act(context.Background(), stuckqueue.AppSonarr, "d", stuckqueue.ActionRegrab, fp)
		return actErr
	}
	seedSnap(t, e, waitingAt(e, time.Hour))
	refusedNoWrite(t, e.sonarr, lower(), "inside the 6 h wait")

	seedSnap(t, e, waitingAt(e, 7*time.Hour))
	if err = lower(); err != nil {
		t.Fatal(err)
	}
	if row := storedRow(t, e); row.State != string(stuckqueue.StateActed) || row.ActedOpID <= 0 {
		t.Fatalf("row = %+v, want the stored D row acted", row)
	}
	rows, err := e.db.ListStuck(context.Background(), "sonarr")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v (err %v), want only the stored D row", rows, err)
	}
}
