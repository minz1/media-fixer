package opexec_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

const (
	pending = operation.ResultPending
	started = operation.ResultStarted
	done    = operation.ResultDone
	failed  = operation.ResultFailed
	unknown = operation.ResultUnknown
)

func TestExecutor_ResultWrittenBeforeNextStepStarts(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(1, 1)))
	got := r.x.Run(context.Background(), r.store.op)
	want := []string{
		"write:started:0", "call:blocklist:1", "write:done:0",
		"write:started:1", "call:delete:1", "write:done:1",
		"write:started:2", "call:search:SeasonSearch", "write:done:2",
	}
	if !slices.Equal(r.j.all(), want) {
		t.Fatalf("journal = %v\nwant      %v", r.j.all(), want)
	}
	if got.Status != operation.Done {
		t.Fatalf("status = %s", got.Status)
	}
	wantStates(t, got, done, done, done)
}

func TestExecutor_StoreFailureStopsBeforeNextCall(t *testing.T) {
	t.Parallel()
	t.Run("started write", func(t *testing.T) {
		t.Parallel()
		r := newRig(mustOp(t, replaceSpec(1, 1)))
		r.store.failOn = "started:1"
		got := r.x.Run(context.Background(), r.store.op)
		if r.j.has("call:delete:1") {
			t.Fatalf("delete called after a failed started write: %v", r.j.all())
		}
		if got.Status != operation.Running {
			t.Fatalf("status = %s, want running", got.Status)
		}
		wantStates(t, got, done, pending, pending)
	})
	t.Run("finished write", func(t *testing.T) {
		t.Parallel()
		r := newRig(mustOp(t, replaceSpec(1, 1)))
		r.store.failOn = "done:0"
		got := r.x.Run(context.Background(), r.store.op)
		if !r.j.has("call:blocklist:1") || r.j.has("write:started:1") {
			t.Fatalf("journal = %v", r.j.all())
		}
		if got.Status != operation.Running {
			t.Fatalf("status = %s, want running", got.Status)
		}
		wantStates(t, got, started, pending, pending)
	})
}

func TestExecutor_TimeoutIsUnknown(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(1, 0)))
	r.x.StepTimeout = 20 * time.Millisecond
	r.arr.block["blocklist:1"] = true
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, unknown, pending)
	if got.Status != operation.FailedAtStep || got.Cause != operation.CauseError {
		t.Fatalf("status = %s cause = %s", got.Status, got.Cause)
	}
}

func TestExecutor_5xxIsUnknown(t *testing.T) {
	t.Parallel()
	for _, code := range []int{500, 502, 408, 409, 429} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()
			r := newRig(mustOp(t, replaceSpec(1, 0)))
			r.arr.fail["blocklist:1"] = status(code)
			got := r.x.Run(context.Background(), r.store.op)
			wantStates(t, got, unknown, pending)
			if got.Results[0].Err == "" {
				t.Fatal("error text not recorded")
			}
		})
	}
}

func TestExecutor_4xxIsFailed(t *testing.T) {
	t.Parallel()
	for _, code := range []int{400, 403, 422} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			t.Parallel()
			r := newRig(mustOp(t, replaceSpec(0, 1)))
			r.arr.fail["delete:1"] = status(code)
			got := r.x.Run(context.Background(), r.store.op)
			wantStates(t, got, failed, pending)
		})
	}
}

func TestExecutor_Blocklist404IsFailed(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(1, 0)))
	r.arr.fail["blocklist:1"] = status(404)
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, failed, pending)
}

func TestExecutor_DeleteFile404ConfirmedGoneIsDone(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(0, 1)))
	r.arr.fail["delete:1"] = notFound()
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, done, done)
	if !strings.Contains(got.Results[0].Note, "gone") {
		t.Fatalf("note = %q", got.Results[0].Note)
	}
	if !r.j.has("read:files") {
		t.Fatalf("journal = %v", r.j.all())
	}
}

func TestExecutor_DeleteFile404StillListedIsFailed(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(0, 1)))
	r.arr.fail["delete:1"] = notFound()
	r.arr.epFiles = []client.EpisodeFile{{ID: 1, Path: "/tv/e1.mkv", Size: 100}}
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, failed, pending)
}

func TestExecutor_DeleteFile404ReadErrorIsUnknown(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(0, 1)))
	r.arr.fail["delete:1"] = notFound()
	r.arr.filesErr = status(500)
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, unknown, pending)
}

func TestExecutor_QueueRemove404IsUnknown(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, regrabSpec()))
	r.arr.fail["queue:5"] = notFound()
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, unknown, pending)
}

func TestExecutor_SearchErrorStopsAtSearch(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(1, 1)))
	r.arr.fail["search:SeasonSearch"] = status(400)
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, done, done, failed)
	if got.Status != operation.FailedAtStep || got.Index != 2 {
		t.Fatalf("status = %s index = %d", got.Status, got.Index)
	}
}

func TestExecutor_CallsUseDetachedContext(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(1, 0)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var callErr error
	var hasDeadline bool
	r.arr.hook = func(c context.Context, label string) {
		if label != "blocklist:1" {
			return
		}
		cancel()
		callErr = c.Err()
		_, hasDeadline = c.Deadline()
	}
	got := r.x.Run(ctx, r.store.op)
	if callErr != nil || !hasDeadline {
		t.Fatalf("call ctx err = %v, deadline = %t", callErr, hasDeadline)
	}
	if r.store.ctxErrs != 0 || r.store.noDeadline != 0 {
		t.Fatalf("store ctx cancelled %d times, without deadline %d times", r.store.ctxErrs, r.store.noDeadline)
	}
	if got.Status != operation.Done {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestExecutor_QueueRemoveUsesProbedQueueID(t *testing.T) {
	t.Parallel()
	t.Run("probed id wins", func(t *testing.T) {
		t.Parallel()
		op := mustOp(t, regrabSpec())
		op.Results[0].QueueID = 77
		r := newRig(op)
		r.x.Run(context.Background(), r.store.op)
		if r.arr.removedID != 77 {
			t.Fatalf("removed %d, want 77", r.arr.removedID)
		}
		if r.arr.flags != "false/true/true" {
			t.Fatalf("flags = %s", r.arr.flags)
		}
	})
	t.Run("step id otherwise", func(t *testing.T) {
		t.Parallel()
		r := newRig(mustOp(t, regrabSpec()))
		r.x.Run(context.Background(), r.store.op)
		if r.arr.removedID != 5 {
			t.Fatalf("removed %d, want 5", r.arr.removedID)
		}
	})
}

func TestS9_4_DeleteErrorMidListStopsAtThatStep(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(0, 3)))
	r.arr.fail["delete:2"] = status(500)
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, done, unknown, pending, pending)
	if r.j.has("call:delete:3") || r.j.has("call:search:SeasonSearch") {
		t.Fatalf("calls after the failed step: %v", r.j.all())
	}
	line := operation.Line(got)
	if !strings.Contains(line, "1 may be deleted") || !strings.Contains(line, "search not sent") {
		t.Fatalf("line = %q", line)
	}
}

func TestS9_6_SearchErrorAfterDeletesStopsAtSearch(t *testing.T) {
	t.Parallel()
	r := newRig(mustOp(t, replaceSpec(0, 2)))
	r.arr.fail["search:SeasonSearch"] = status(503)
	got := r.x.Run(context.Background(), r.store.op)
	wantStates(t, got, done, done, unknown)
	if got.Status != operation.FailedAtStep || !operation.Resumable(got) {
		t.Fatalf("status = %s resumable = %t", got.Status, operation.Resumable(got))
	}
	if !strings.Contains(operation.Line(got), "deleted 2 of 2 files") {
		t.Fatalf("line = %q", operation.Line(got))
	}
}
