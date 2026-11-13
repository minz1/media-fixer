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
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 2)))
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo, operation.VerdictTodo)
	if got[0].Index != 0 || got[1].Index != 1 || got[2].Index != 2 {
		t.Fatalf("indexes = %+v", got)
	}
}

func TestProbe_ChangedPathIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.epFiles = []client.EpisodeFile{{ID: 1, Path: "/tv/other.mkv", Size: 100}}
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 1)))
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if !strings.Contains(got[0].Note, "changed since approval") {
		t.Fatalf("note = %q", got[0].Note)
	}
}

func TestProbe_SizeChangedIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.epFiles = []client.EpisodeFile{{ID: 1, Path: "/tv/e1.mkv", Size: 999}}
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 1)))
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
}

func TestProbe_PendingDeleteNeverSkipsSearch(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.epFiles = []client.EpisodeFile{{ID: 1, Path: "/tv/e1.mkv", Size: 100}}
	r.arr.queue = []client.QueueEntry{{ID: 9, DownloadID: "dl-up"}}
	op := interruptedAt(t, replaceSpec(0, 1))
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
	got := probeOf(t, r, interruptedAt(t, regrabSpec()))
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo)
}

func TestProbe_MediaGoneIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.seriesErr = notFound()
	got := probeOf(t, r, interruptedAt(t, replaceSpec(0, 0)))
	wantVerdicts(t, got, operation.VerdictConflict)
}

func TestProbe_BlocklistFoundInHistoryIsDone(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.failedHit = true
	got := probeOf(t, r, interruptedAt(t, replaceSpec(1, 0)))
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
	got := probeOf(t, r, interruptedAt(t, spec))
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
	got := probeOf(t, r, interruptedAt(t, regrabSpec()))
	wantVerdicts(t, got, operation.VerdictTodo, operation.VerdictTodo)
	if got[0].QueueID != 99 {
		t.Fatalf("queue id = %d", got[0].QueueID)
	}
}

func TestProbe_QueueGoneWithFailedRecordIsDone(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.failedHit = true
	got := probeOf(t, r, interruptedAt(t, regrabSpec()))
	wantVerdicts(t, got, operation.VerdictDone, operation.VerdictTodo)
	if r.arr.failedArgs != "tv/5/dl-q/2026-11-07T12:00:00Z" {
		t.Fatalf("args = %s", r.arr.failedArgs)
	}
}

func TestProbe_QueueGoneWithoutFailedRecordIsConflict(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	got := probeOf(t, r, interruptedAt(t, regrabSpec()))
	wantVerdicts(t, got, operation.VerdictConflict, operation.VerdictTodo)
	if !strings.Contains(got[0].Note, "left the queue without a blocklist record") {
		t.Fatalf("note = %q", got[0].Note)
	}
}

func TestProbe_ReadErrorReturnsError(t *testing.T) {
	t.Parallel()
	r := newRig(operation.Op{})
	r.arr.filesErr = status(500)
	if _, err := r.x.Probe(context.Background(), interruptedAt(t, replaceSpec(0, 1))); err == nil {
		t.Fatal("want an error")
	}
}
