package opexec_test

import (
	"context"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

func probeOf(t *testing.T, r *rig, op operation.Op) []operation.Probe {
	t.Helper()
	got, err := r.x.Probe(context.Background(), op)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return got
}

func wantVerdicts(t *testing.T, got []operation.Probe, want ...operation.Verdict) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("probes = %+v, want %v", got, want)
	}
	for i, p := range got {
		if p.Verdict != want[i] {
			t.Fatalf("probe %d = %s (%q), want %s", i, p.Verdict, p.Note, want[i])
		}
	}
}

func TestProbe_DeletedFileIDIsDone(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.epFiles = []client.EpisodeFile{{ID: 2, Path: "/tv/e2.mkv", Size: 100}}
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 2), 0))
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo, operation.VerdictTodo)
	if got[0].Index != 0 || got[1].Index != 1 || got[2].Index != 2 {
		t.Fatalf("indexes = %+v", got)
	}
}

func TestProbe_ChangedPathIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.epFiles = []client.EpisodeFile{{ID: 1, Path: "/tv/other.mkv", Size: 100}}
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 1), 0))
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if !strings.Contains(got[0].Note, "changed since approval") {
		t.Fatalf("note = %q", got[0].Note)
	}
}

func TestProbe_SizeChangedIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.epFiles = []client.EpisodeFile{{ID: 1, Path: "/tv/e1.mkv", Size: 999}}
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 1), 0))
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
}

func TestProbe_PendingDeleteNeverSkipsSearch(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.epFiles = []client.EpisodeFile{{ID: 1, Path: "/tv/e1.mkv", Size: 100}}
	r.arr.queue = []client.QueueEntry{{ID: 9, DownloadID: "dl-up"}}
	op := interruptedAt(t, replaceSpec(0, 1), 0)
	got := probeOf(t, r, op)
	wantVerdicts(t, got, operation.VerdictTodo, operation.VerdictTodo)
	if r.j.has("read:queue") {
		t.Fatalf("search probe read the queue: %v", r.j.all())
	}
}

func TestProbe_RegrabUpgradeStillSearches(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.failedHit = true
	r.arr.movies = []client.Movie{{ID: 5, HasFile: true}}
	got := probeOf(t, r, interruptedAt(t, regrabSpec(), 0))
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo)
}

func TestProbe_MediaGoneIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.seriesErr = notFound()
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 0), 0))
	wantVerdicts(t, got, operation.VerdictConflict)
}

func TestProbe_TorrentPresentIsDone(t *testing.T) {
	t.Parallel()
	t.Run("add present", func(t *testing.T) {
		t.Parallel()
		r := newRig(operation.Op{})
		r.decy.present = true
		got := probeOf(t, r, interruptedAt(t, readdSpec(), 1))
		wantVerdicts(t, got, operation.VerdictDone)
		if !strings.Contains(got[0].Note, "already re-added") {
			t.Fatalf("note = %q", got[0].Note)
		}
	})
	t.Run("delete present", func(t *testing.T) {
		t.Parallel()
		r := newRig(operation.Op{})
		r.decy.present = true
		got := probeOf(t, r, interruptedAt(t, readdSpec(), 0))
		wantVerdicts(t, got, operation.VerdictTodo, operation.VerdictTodo)
	})
	t.Run("delete absent", func(t *testing.T) {
		t.Parallel()
		r := newRig(operation.Op{})
		got := probeOf(t, r, interruptedAt(t, readdSpec(), 0))
		wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo)
	})
}

func TestProbe_BlocklistFoundInHistoryIsDone(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.failedHit = true
	got := probeOf(t, r, interruptedAt(t, replaceSpec(1, 0), 0))
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo)
	if r.arr.failedArgs != "tv/5/dl1/2026-11-07T12:00:00Z" {
		t.Fatalf("args = %s", r.arr.failedArgs)
	}
}

func TestProbe_BlocklistWithoutDownloadIDIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	spec := replaceSpec(1, 0)
	spec.Steps[0].DownloadID = ""
	got := probeOf(t, r, interruptedAt(t, spec, 0))
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if !strings.Contains(got[0].Note, "has no download id") || !strings.Contains(got[0].Note, "Rerun") {
		t.Fatalf("note = %q", got[0].Note)
	}
	if r.j.has("read:history") {
		t.Fatal("history read without a download id")
	}
}

func TestProbe_QueueStillQueuedIsTodoWithCurrentID(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.queue = []client.QueueEntry{{ID: 3, DownloadID: "other"}, {ID: 99, DownloadID: "DL-Q"}}
	got := probeOf(t, r, interruptedAt(t, regrabSpec(), 0))
	wantVerdicts(t, got, operation.VerdictTodo, operation.VerdictTodo)
	if got[0].QueueID != 99 {
		t.Fatalf("queue id = %d", got[0].QueueID)
	}
}

func TestProbe_QueueGoneWithFailedRecordIsDone(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.failedHit = true
	got := probeOf(t, r, interruptedAt(t, regrabSpec(), 0))
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo)
	if r.arr.failedArgs != "tv/5/dl-q/2026-11-07T12:00:00Z" {
		t.Fatalf("args = %s", r.arr.failedArgs)
	}
}

func TestProbe_QueueGoneWithoutFailedRecordIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	got := probeOf(t, r, interruptedAt(t, regrabSpec(), 0))
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if !strings.Contains(got[0].Note, "left the queue without a blocklist record") {
		t.Fatalf("note = %q", got[0].Note)
	}
}

func TestProbe_ReadErrorReturnsError(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.filesErr = status(500)
	if _, err := r.x.Probe(context.Background(), interruptedAt(t, replaceSpec(0, 1), 0)); err == nil {
		t.Fatal("want an error")
	}
	r2 := newRig(operation.Op{})
	r2.decy.presentErr = status(500)
	if _, err := r2.x.Probe(context.Background(), interruptedAt(t, readdSpec(), 0)); err == nil {
		t.Fatal("want an error")
	}
}

func swapProbe(t *testing.T, r *rig) []operation.Probe {
	t.Helper()
	return probeOf(t, r, interruptedAt(t, mustSwapSpec(t), 0))
}

func TestProbe_ImportDone(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	setFile(r.arr, 42, swapSize)
	got := swapProbe(t, r)
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo)
	if got[0].Index != 0 || got[1].Index != 1 {
		t.Fatalf("indexes = %+v", got)
	}
	if r.j.has("call:import") {
		t.Fatalf("probe posted an import: %v", r.j.all())
	}
}

func TestProbe_ImportTodo(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	wantVerdicts(t, swapProbe(t, r), operation.VerdictTodo, operation.VerdictTodo)
}

func TestProbe_ImportCandidateGone(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.cands = nil
	got := swapProbe(t, r)
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if want := "the candidate is no longer under the downloads folder; preview again"; got[0].Note != want {
		t.Fatalf("note = %q, want %q", got[0].Note, want)
	}
}

func TestProbe_ImportThirdFile(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	setFile(r.arr, 50, 777)
	got := swapProbe(t, r)
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if want := "S01E06 has a different file since approval"; got[0].Note != want {
		t.Fatalf("note = %q, want %q", got[0].Note, want)
	}
}

func TestProbe_ImportNoFile(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.episodes[0].EpisodeFileID = 0
	got := swapProbe(t, r)
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if want := "Sonarr ran the import but S01E06 has no file; search from Sonarr."; got[0].Note != want {
		t.Fatalf("note = %q, want %q", got[0].Note, want)
	}
}

func TestProbe_NotifyAlwaysTodo(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	op := interruptedAt(t, mustSwapSpec(t), 1)
	wantVerdicts(t, probeOf(t, r, op), operation.VerdictTodo)
	if len(r.j.all()) != 0 {
		t.Fatalf("notify probe touched a service: %v", r.j.all())
	}
}

func TestProbe_ImportStillRunning(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.pending = true
	got := swapProbe(t, r)
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if want := "an import of this file is still running; resume later"; got[0].Note != want {
		t.Fatalf("note = %q, want %q", got[0].Note, want)
	}
}
