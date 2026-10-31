package stuckqueue_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/stuckqueue"
)

func act(e *runEnv, s *stuckqueue.Scheduler, a stuckqueue.App, dl string, action stuckqueue.Action) (string, error) {
	f := e.sonarr
	if a == stuckqueue.AppRadarr {
		f = e.radarr
	}
	targets := ""
	if entries, err := f.client().QueueEntries(context.Background()); err == nil {
		for _, d := range stuckqueue.Group(a, entries) {
			if d.DownloadID == dl {
				targets = d.Targets()
			}
		}
	}
	if _, err := e.db.UpsertStuckSeen(
		context.Background(),
		string(a),
		dl,
		dl,
		"",
		e.clock.Now().Add(-7*time.Hour),
	); err != nil {
		return "", err
	}
	return actSeen(s, a, dl, action, targets)
}

func actSeen(
	s *stuckqueue.Scheduler,
	a stuckqueue.App,
	dl string,
	action stuckqueue.Action,
	seen string,
) (string, error) {
	return s.Act(context.Background(), a, dl, action, card.Fingerprint([]byte(seen)))
}

func refusedNoWrite(t *testing.T, f *fakeArr, err error, reason string) {
	t.Helper()
	if !errors.Is(err, stuckqueue.ErrRefused) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("err = %v, want refusal containing %q", err, reason)
	}
	if ws := f.writesCopy(); len(ws) != 0 {
		t.Fatalf("writes = %+v, want none", ws)
	}
}

func TestAct_RefusesGone(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	_, err := act(e, s, stuckqueue.AppSonarr, "GONE", stuckqueue.ActionRemove)
	refusedNoWrite(t, e.sonarr, err, "no longer in the Sonarr queue")
}

func TestAct_RefusesNotStuck(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	busy := rawRec(2, "D", "Pack", 2, false)
	busy["status"], busy["trackedDownloadState"] = "downloading", "downloading"
	e.sonarr.set(rawRec(1, "D", "Pack", 1, false, sample), busy)
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove)
	refusedNoWrite(t, e.sonarr, err, "no longer stuck")
}

func TestAct_RefusesActionNotOffered(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "AHS", 12, true, sample))
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport)
	refusedNoWrite(t, e.sonarr, err, "not offered")
}

func TestAct_ImportRechecksGuard(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	e.sonarr.setCandidates("D", candidateJSON("D", 1, true))
	_, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport)
	refusedNoWrite(t, e.sonarr, err, "already has a file")
}

func TestAct_Import(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	e.sonarr.setCandidates("D", candidateJSON("D", 1, false))
	notice, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport)
	if err != nil || notice != "Import started for S01E01." {
		t.Fatalf("notice = %q, err = %v", notice, err)
	}
	ws := e.sonarr.writesCopy()
	if len(ws) != 1 || ws[0].Path != "/api/v3/command" || ws[0].Body["name"] != "ManualImport" {
		t.Fatalf("writes = %+v", ws)
	}
	if files, _ := ws[0].Body["files"].([]any); len(files) != 1 {
		t.Fatalf("files = %v", ws[0].Body["files"])
	}
}

func TestAct_Regrab(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "Pack", 1, false, sample), rawRec(2, "D", "Pack", 2, false, sample))
	notice, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab)
	if err != nil || notice != "Re-grab started for S01E01, S01E02." {
		t.Fatalf("notice = %q, err = %v", notice, err)
	}
	ws := e.sonarr.writesCopy()
	if len(ws) != 2 || ws[0].Method != http.MethodDelete {
		t.Fatalf("writes = %+v", ws)
	}
	q := ws[0].Query
	if q.Get("removeFromClient") != "false" || q.Get("blocklist") != "true" || q.Get("skipRedownload") != "true" {
		t.Fatalf("delete query = %v", q)
	}
	if ws[1].Body["name"] != "EpisodeSearch" {
		t.Fatalf("search = %+v", ws[1].Body)
	}
	ids, _ := ws[1].Body["episodeIds"].([]any)
	if !slices.Equal(ids, []any{float64(1), float64(2)}) {
		t.Fatalf("episodeIds = %v", ids)
	}
}

func TestAct_RegrabRadarr(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.radarr.set(rawMovie(1, "M", "Movie", 5, false, sample))
	if _, err := act(e, s, stuckqueue.AppRadarr, "M", stuckqueue.ActionRegrab); err != nil {
		t.Fatal(err)
	}
	ws := e.radarr.writesCopy()
	if len(ws) != 2 || ws[1].Body["name"] != "MoviesSearch" {
		t.Fatalf("writes = %+v", ws)
	}
	if ids, _ := ws[1].Body["movieIds"].([]any); !slices.Equal(ids, []any{float64(5)}) {
		t.Fatalf("movieIds = %v", ws[1].Body["movieIds"])
	}
}

func TestAct_RegrabSearchFails(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.radarr.set(rawMovie(1, "M", "Movie", 5, false, sample))
	e.radarr.failCommand["MoviesSearch"] = true
	_, err := act(e, s, stuckqueue.AppRadarr, "M", stuckqueue.ActionRegrab)
	if err == nil || !strings.Contains(err.Error(), "blocklisted") ||
		!strings.Contains(err.Error(), "search from Radarr") {
		t.Fatalf("err = %v", err)
	}
}

func TestAct_InsideWaitOnlyRemoves(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	_, err := actSeen(s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab, "S01E01")
	refusedNoWrite(t, e.sonarr, err, "inside the 6 h wait")
	if _, err = actSeen(s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, "S01E01"); err != nil {
		t.Fatalf("remove inside the wait: %v", err)
	}
}

func TestAct_RefusesUnseen(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	_, err := actSeen(s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRegrab, "S01E01")
	refusedNoWrite(t, e.sonarr, err, "no run has checked")
}

func TestAct_Remove(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "One Piece", 0, false))
	notice, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove)
	if err != nil || notice != "Removed." {
		t.Fatalf("notice = %q, err = %v", notice, err)
	}
	ws := e.sonarr.writesCopy()
	if len(ws) != 1 {
		t.Fatalf("writes = %+v", ws)
	}
	q := ws[0].Query
	if q.Get("removeFromClient") != "false" || q.Get("blocklist") != "false" || q.Get("skipRedownload") != "true" {
		t.Fatalf("query = %v", q)
	}
}

func TestAct_DropsFromLast(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	e.clock.Add(6 * time.Hour)
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if res, _, _ := s.State(); len(app(res, stuckqueue.AppSonarr).Items) != 1 {
		t.Fatalf("setup: last = %+v", res)
	}
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := s.State(); len(app(res, stuckqueue.AppSonarr).Items) != 0 {
		t.Fatalf("last still lists it: %+v", res)
	}
}

func TestAct_DropsWaitingFromLast(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "NOVA", 1, false, sample))
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if res, _, _ := s.State(); len(app(res, stuckqueue.AppSonarr).Waiting) != 1 {
		t.Fatalf("setup: last = %+v", res)
	}
	if _, err := act(e, s, stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := s.State(); len(app(res, stuckqueue.AppSonarr).Waiting) != 0 {
		t.Fatalf("last still lists it as waiting: %+v", res)
	}
}

func TestAct_UnconfiguredApp(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.runner.Radarr = nil
	_, err := act(e, s, stuckqueue.AppRadarr, "M", stuckqueue.ActionRemove)
	refusedNoWrite(t, e.radarr, err, "not configured")
}

func TestAct_RefusesDuringRun(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	gate := e.sonarr.hold()
	if err := s.RunNow(); err != nil {
		t.Fatal(err)
	}
	_, err := act(e, s, stuckqueue.AppRadarr, "M", stuckqueue.ActionRemove)
	close(gate)
	s.Wait()
	refusedNoWrite(t, e.radarr, err, "a run is active")
}

func TestAct_RefusesRemappedTargets(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "Link Click", 2, false, sample))
	e.sonarr.setCandidates("D", candidateJSON("D", 2, false))
	_, err := actSeen(s, stuckqueue.AppSonarr, "D", stuckqueue.ActionImport, "S01E01")
	refusedNoWrite(t, e.sonarr, err, "now maps to S01E02")
}

func TestAct_RefusesOnTargetsFingerprintMismatch(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "D", "Link Click", 2, false, sample))
	if _, err := e.db.UpsertStuckSeen(
		context.Background(), string(stuckqueue.AppSonarr), "D", "D", "", e.clock.Now().Add(-7*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	const stale = "0123456789ab"
	_, err := s.Act(context.Background(), stuckqueue.AppSonarr, "D", stuckqueue.ActionRemove, stale)
	refusedNoWrite(t, e.sonarr, err, "now maps to S01E02")
	if strings.Contains(err.Error(), stale) {
		t.Fatalf("refusal %q shows the fingerprint", err)
	}
}

func TestAct_ForgetLeavesHandedOutResultsAlone(t *testing.T) {
	t.Parallel()
	e, s := newSched(t)
	e.sonarr.set(rawRec(1, "A", "NOVA A", 1, false, sample), rawRec(2, "B", "NOVA B", 2, false, sample))
	for range 2 {
		if err := s.RunNow(); err != nil {
			t.Fatal(err)
		}
		s.Wait()
		e.clock.Add(6 * time.Hour)
	}
	before, _, _ := s.State()
	if _, err := act(e, s, stuckqueue.AppSonarr, "A", stuckqueue.ActionRemove); err != nil {
		t.Fatal(err)
	}
	items := app(before, stuckqueue.AppSonarr).Items
	if len(items) != 2 || items[0].DownloadID != "A" || items[1].DownloadID != "B" {
		t.Fatalf("a Result handed out earlier changed under its holder: %+v", items)
	}
}
