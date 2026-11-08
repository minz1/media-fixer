package stuckqueue_test

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func storeT0() time.Time { return time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC) }

func sonarrKey(id string) stuckqueue.Key {
	return stuckqueue.Key{App: stuckqueue.AppSonarr, DownloadID: id}
}

func everyStoredSnapshot() []stuckqueue.Snapshot {
	t0 := storeT0()
	seen := t0.Add(-7 * time.Hour)
	files := stuckqueue.Files{N: 2, M: 3}
	return []stuckqueue.Snapshot{
		{Key: sonarrKey("W"), State: stuckqueue.StateWaiting, FirstSeen: seen, Misses: 1, TargetsFP: "fp",
			Offered: stuckqueue.OfferRemove},
		{
			Key:       sonarrKey("D"),
			State:     stuckqueue.StateDue,
			FirstSeen: seen,
			TargetsFP: "fp",
			Offered:   stuckqueue.OfferAll,
		},
		{Key: sonarrKey("H"), State: stuckqueue.StateHeld, FirstSeen: seen, Offered: stuckqueue.OfferRemove},
		{Key: sonarrKey("N"), State: stuckqueue.StateNotified, FirstSeen: seen, NotifiedAt: t0,
			Offered: stuckqueue.OfferRegrabRemove},
		{Key: sonarrKey("NI"), State: stuckqueue.StateNotified, FirstSeen: seen, NotifiedAt: t0,
			Offered: stuckqueue.OfferAll, Acted: &stuckqueue.Acted{
				Action: stuckqueue.ActionImport, At: t0, Result: stuckqueue.ResultPartial, Files: files}},
		{Key: sonarrKey("I"), State: stuckqueue.StateImportInflight, FirstSeen: seen, NotifiedAt: t0,
			Import: &stuckqueue.ImportCmd{CommandID: 42, StartedAt: t0, Files: files}},
		{Key: sonarrKey("AI"), State: stuckqueue.StateActed, FirstSeen: seen, Acted: &stuckqueue.Acted{
			Action: stuckqueue.ActionImport, At: t0, Result: stuckqueue.ResultOK, Files: stuckqueue.Files{N: 1, M: 1}}},
		{Key: sonarrKey("AR"), State: stuckqueue.StateActed, FirstSeen: seen, NotifiedAt: t0, Acted: &stuckqueue.Acted{
			Action: stuckqueue.ActionRemove, At: t0, Result: stuckqueue.ResultOK}},
		{Key: sonarrKey("AU"), State: stuckqueue.StateActed, FirstSeen: seen, Acted: &stuckqueue.Acted{
			Action: stuckqueue.ActionRegrab, At: t0, Result: stuckqueue.ResultUnfinished, OpID: 7}},
		{Key: sonarrKey("AO"), State: stuckqueue.StateActed, FirstSeen: seen, Acted: &stuckqueue.Acted{
			Action: stuckqueue.ActionRegrab, At: t0, Result: stuckqueue.ResultOK, OpID: 8}},
		{Key: sonarrKey("C"), State: stuckqueue.StateCleared, FirstSeen: seen, Offered: stuckqueue.OfferRemove},
	}
}

func TestStore_RoundTripEveryState(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	ctx := context.Background()
	snaps := everyStoredSnapshot()
	for _, s := range snaps {
		if err := stuckqueue.Check(s); err != nil {
			t.Fatalf("sample %s is illegal: %v", s.Key.DownloadID, err)
		}
		if got := stuckqueue.FromRowForTest(stuckqueue.ToRowForTest(s)); !reflect.DeepEqual(got, s) {
			t.Fatalf("in memory %s:\n got %+v\nwant %+v", s.Key.DownloadID, got, s)
		}
	}
	err := e.db.ApplyStuck(ctx, "sonarr", nil, func(map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
		put := make([]db.StuckRow, 0, len(snaps))
		for _, s := range snaps {
			put = append(put, stuckqueue.ToRowForTest(s))
		}
		return put, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range snaps {
		row, ok, gerr := e.db.GetStuck(ctx, "sonarr", s.Key.DownloadID)
		if gerr != nil || !ok {
			t.Fatalf("%s: ok %v err %v", s.Key.DownloadID, ok, gerr)
		}
		if got := stuckqueue.FromRowForTest(row); !reflect.DeepEqual(got, s) {
			t.Fatalf("through the db %s:\n got %+v\nwant %+v", s.Key.DownloadID, got, s)
		}
	}
}

func seedRows(t *testing.T, d *db.DB, rows ...db.StuckRow) {
	t.Helper()
	err := d.ApplyStuck(
		context.Background(),
		"sonarr",
		nil,
		func(map[string]*db.StuckRow) ([]db.StuckRow, []string, error) {
			return rows, nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func seenEvent(at time.Time) stuckqueue.Event {
	return stuckqueue.Event{
		Kind: stuckqueue.EvSeen, At: at, MinAge: 6 * time.Hour,
		Obs: stuckqueue.Observation{FP: "new", Verdict: stuckqueue.VerdictGap, Offered: stuckqueue.OfferRegrabRemove},
	}
}

func TestStore_OneBadRowDoesNotBlockTheApp(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	ctx := context.Background()
	t0 := storeT0()
	good := stuckqueue.ToRowForTest(stuckqueue.Snapshot{
		Key: sonarrKey("GOOD"), State: stuckqueue.StateNotified, FirstSeen: t0.Add(-8 * time.Hour),
		NotifiedAt: t0.Add(-time.Hour), Offered: stuckqueue.OfferRemove,
	})
	bad := good
	bad.DownloadID, bad.FirstSeen, bad.NotifiedAt = "BAD", t0.Add(-time.Hour), t0.Add(-8*time.Hour)
	seedRows(t, e.db, good, bad)
	snaps, _, err := stuckqueue.ApplyForTest(ctx, e.runner, stuckqueue.AppSonarr, map[stuckqueue.Key]stuckqueue.Event{
		sonarrKey("GOOD"): seenEvent(t0), sonarrKey("BAD"): seenEvent(t0),
	})
	if err != nil {
		t.Fatalf("one bad row failed the app: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots %d, want 2", len(snaps))
	}
	gotGood, _, _ := e.db.GetStuck(ctx, "sonarr", "GOOD")
	if gotGood.TargetsFP != "new" || gotGood.Offered != string(stuckqueue.OfferRegrabRemove) {
		t.Fatalf("good row not committed: %+v", gotGood)
	}
	gotBad, _, _ := e.db.GetStuck(ctx, "sonarr", "BAD")
	if !reflect.DeepEqual(gotBad, bad) {
		t.Fatalf("bad row changed:\n got %+v\nwant %+v", gotBad, bad)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "BAD") {
		t.Fatalf("no ERROR naming the bad row: %s", logs)
	}
}

func TestStore_RejectSkipsTheRowAtInfo(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	ctx := context.Background()
	snaps, effs, err := stuckqueue.ApplyForTest(
		ctx,
		e.runner,
		stuckqueue.AppSonarr,
		map[stuckqueue.Key]stuckqueue.Event{
			sonarrKey("NEW"): {Kind: stuckqueue.EvMissed, At: storeT0()},
		},
	)
	if err != nil || len(effs) != 0 || len(snaps) != 1 || snaps[0].State != stuckqueue.StateUntracked {
		t.Fatalf("snaps %+v effs %+v err %v", snaps, effs, err)
	}
	if _, ok, _ := e.db.GetStuck(ctx, "sonarr", "NEW"); ok {
		t.Fatal("a rejected event wrote a row")
	}
	if logs := e.logs.String(); !strings.Contains(logs, "level=INFO") || strings.Contains(logs, "level=ERROR") {
		t.Fatalf("logs: %s", logs)
	}
}

func TestStore_UntrackedDeletesTheRow(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	ctx := context.Background()
	t0 := storeT0()
	seedRows(t, e.db, stuckqueue.ToRowForTest(stuckqueue.Snapshot{
		Key: sonarrKey("GONE"), State: stuckqueue.StateNotified, FirstSeen: t0.Add(-8 * time.Hour),
		NotifiedAt: t0.Add(-time.Hour), Misses: 1, Offered: stuckqueue.OfferRemove,
	}))
	snaps, _, err := stuckqueue.ApplyForTest(ctx, e.runner, stuckqueue.AppSonarr, map[stuckqueue.Key]stuckqueue.Event{
		sonarrKey("GONE"): {Kind: stuckqueue.EvMissed, At: t0},
	})
	if err != nil || len(snaps) != 1 || snaps[0].State != stuckqueue.StateUntracked {
		t.Fatalf("snaps %+v err %v", snaps, err)
	}
	if _, ok, _ := e.db.GetStuck(ctx, "sonarr", "GONE"); ok {
		t.Fatal("second miss left the row")
	}
}

func TestStore_EffectsReturnedOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	e := newRunEnv(t)
	ctx := context.Background()
	events := map[stuckqueue.Key]stuckqueue.Event{
		sonarrKey("A"): {Kind: stuckqueue.EvActed, At: storeT0(), Action: stuckqueue.ActionRemove},
	}
	raw, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(ctx, `CREATE TRIGGER refuse_stuck BEFORE INSERT ON stuck_downloads
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	snaps, effs, err := stuckqueue.ApplyForTest(ctx, e.runner, stuckqueue.AppSonarr, events)
	if err == nil || len(effs) != 0 || len(snaps) != 0 {
		t.Fatalf("failed commit: snaps %+v effs %+v err %v", snaps, effs, err)
	}
	if _, err = raw.ExecContext(ctx, `DROP TRIGGER refuse_stuck`); err != nil {
		t.Fatal(err)
	}
	snaps, effs, err = stuckqueue.ApplyForTest(ctx, e.runner, stuckqueue.AppSonarr, events)
	if err != nil || len(snaps) != 1 || snaps[0].State != stuckqueue.StateActed {
		t.Fatalf("snaps %+v err %v", snaps, err)
	}
	if len(effs) != 1 {
		t.Fatalf("effects %+v", effs)
	}
	if _, ok, _ := e.db.GetStuck(ctx, "sonarr", "A"); !ok {
		t.Fatal("acted row not committed")
	}
}
