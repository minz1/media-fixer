package opexec_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

func t0() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

type journal struct {
	mu    sync.Mutex
	lines []string
}

func (j *journal) add(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, s)
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.lines...)
}

func (j *journal) has(s string) bool {
	return slices.Contains(j.all(), s)
}

var errDisk = errors.New("disk full")

type memStore struct {
	j          *journal
	op         operation.Op
	failOn     string
	ctxErrs    int
	noDeadline int
}

func writeLabel(before, after operation.Op) string {
	for i := range after.Results {
		if after.Results[i].State != before.Results[i].State {
			return fmt.Sprintf("%s:%d", after.Results[i].State, i)
		}
	}
	return "status:" + string(after.Status)
}

func (s *memStore) ApplyOperation(
	ctx context.Context,
	_ int64,
	decide func(operation.Op) (operation.Op, error),
) (operation.Op, error) {
	next, err := decide(s.op)
	if err != nil {
		return operation.Op{}, err
	}
	if ctx.Err() != nil {
		s.ctxErrs++
	}
	if _, ok := ctx.Deadline(); !ok {
		s.noDeadline++
	}
	label := writeLabel(s.op, next)
	if label == s.failOn {
		s.j.add("fail:" + label)
		return operation.Op{}, errDisk
	}
	s.j.add("write:" + label)
	s.op = next
	return next, nil
}

type fakeArr struct {
	j          *journal
	fail       map[string]error
	block      map[string]bool
	hook       func(ctx context.Context, label string)
	epFiles    []client.EpisodeFile
	movieFiles []client.MovieFile
	filesErr   error
	seriesErr  error
	movies     []client.Movie
	moviesErr  error
	queue      []client.QueueEntry
	queueErr   error
	failedHit  bool
	failedErr  error
	failedArgs string
	removedID  int
	flags      string
}

func (a *fakeArr) step(ctx context.Context, label string) error {
	a.j.add("call:" + label)
	if a.hook != nil {
		a.hook(ctx, label)
	}
	if a.block[label] {
		<-ctx.Done()
		return ctx.Err()
	}
	return a.fail[label]
}

func (a *fakeArr) MarkHistoryFailed(ctx context.Context, id int) error {
	return a.step(ctx, fmt.Sprintf("blocklist:%d", id))
}

func (a *fakeArr) DeleteEpisodeFile(ctx context.Context, id int) error {
	return a.step(ctx, fmt.Sprintf("delete:%d", id))
}

func (a *fakeArr) DeleteMovieFile(ctx context.Context, id int) error {
	return a.step(ctx, fmt.Sprintf("delete:%d", id))
}

func (a *fakeArr) Search(ctx context.Context, command string, _, _ int, _ []int) error {
	return a.step(ctx, "search:"+command)
}

func (a *fakeArr) RemoveQueueItem(ctx context.Context, id int, fromClient, blocklist, skip bool) error {
	a.removedID = id
	a.flags = fmt.Sprintf("%t/%t/%t", fromClient, blocklist, skip)
	return a.step(ctx, fmt.Sprintf("queue:%d", id))
}

func (a *fakeArr) GetEpisodeFiles(context.Context, int) ([]client.EpisodeFile, error) {
	a.j.add("read:files")
	return a.epFiles, a.filesErr
}

func (a *fakeArr) GetMovieFiles(context.Context, int) ([]client.MovieFile, error) {
	a.j.add("read:files")
	return a.movieFiles, a.filesErr
}

func (a *fakeArr) GetSeries(_ context.Context, id int) (client.Series, error) {
	a.j.add("read:series")
	return client.Series{ID: id}, a.seriesErr
}

func (a *fakeArr) ListMovies(context.Context) ([]client.Movie, error) {
	a.j.add("read:movies")
	return a.movies, a.moviesErr
}

func (a *fakeArr) QueueEntries(context.Context) ([]client.QueueEntry, error) {
	a.j.add("read:queue")
	return a.queue, a.queueErr
}

func (a *fakeArr) FailedForDownload(
	_ context.Context,
	mediaType string,
	mediaID int,
	downloadID string,
	since time.Time,
) (bool, error) {
	a.j.add("read:history")
	a.failedArgs = fmt.Sprintf("%s/%d/%s/%s", mediaType, mediaID, downloadID, since.UTC().Format(time.RFC3339))
	return a.failedHit, a.failedErr
}

type fakeDecy struct {
	j          *journal
	fail       map[string]error
	present    bool
	presentErr error
}

func (d *fakeDecy) DeleteTorrent(_ context.Context, _, hash string, _ bool) error {
	d.j.add("call:tdelete:" + hash)
	return d.fail["tdelete"]
}

func (d *fakeDecy) AddTorrent(_ context.Context, _, _ string) error {
	d.j.add("call:tadd")
	return d.fail["tadd"]
}

func (d *fakeDecy) TorrentByHash(context.Context, string) (bool, error) {
	d.j.add("read:torrent")
	return d.present, d.presentErr
}

type rig struct {
	j     *journal
	store *memStore
	arr   *fakeArr
	decy  *fakeDecy
	x     *opexec.Executor
}

func newRig(op operation.Op) *rig {
	j := &journal{}
	r := &rig{
		j:     j,
		store: &memStore{j: j, op: op},
		arr:   &fakeArr{j: j, fail: map[string]error{}, block: map[string]bool{}},
		decy:  &fakeDecy{j: j, fail: map[string]error{}},
	}
	tick := 0
	r.x = &opexec.Executor{
		Store: r.store,
		Ports: opexec.Ports{Arr: func(string) opexec.Arr { return r.arr }, Decypharr: r.decy},
		Now: func() time.Time {
			tick++
			return t0().Add(time.Duration(tick) * time.Second)
		},
		StepTimeout: 2 * time.Second,
	}
	return r
}

func blocklistStep(n int) operation.Step {
	return operation.Step{
		Kind: operation.StepBlocklist, App: "sonarr", HistoryID: n, DownloadID: fmt.Sprintf("dl%d", n),
		GrabbedAt: t0(), MediaID: 5,
	}
}

func deleteStep(n int) operation.Step {
	return operation.Step{
		Kind: operation.StepDeleteFile, App: "sonarr", FileID: n, Path: fmt.Sprintf("/tv/e%d.mkv", n), Size: 100,
		MediaID: 5,
	}
}

func searchStep() operation.Step {
	return operation.Step{Kind: operation.StepSearch, App: "sonarr", Command: "SeasonSearch", MediaID: 5, Season: 1}
}

func incident() operation.Subject {
	return operation.Subject{Kind: operation.SubjectIncident, ID: "inc-1"}
}

func replaceSpec(blocks, deletes int) operation.Spec {
	var steps []operation.Step
	for i := 1; i <= blocks; i++ {
		steps = append(steps, blocklistStep(i))
	}
	for i := 1; i <= deletes; i++ {
		steps = append(steps, deleteStep(i))
	}
	return operation.Spec{
		Subject: incident(), Kind: operation.KindReplace, Title: "Show S01", Steps: append(steps, searchStep()),
	}
}

func readdSpec() operation.Spec {
	return operation.Spec{
		Subject: incident(), Kind: operation.KindTorrentReadd, Title: "Show",
		Steps: []operation.Step{
			{Kind: operation.StepTorrentDelete, InfoHash: "abc", Category: "tv", Name: "Show"},
			{Kind: operation.StepTorrentAdd, InfoHash: "abc", Category: "tv", Name: "Show", Magnet: "magnet:?xt=abc"},
		},
	}
}

func regrabSpec() operation.Spec {
	return operation.Spec{
		Subject: operation.Subject{Kind: operation.SubjectDownload, ID: "dl-q"}, Kind: operation.KindRegrab,
		Title: "Show S01E01",
		Steps: []operation.Step{
			{
				Kind: operation.StepQueueRemove, App: "sonarr", QueueID: 5, DownloadID: "dl-q", MediaID: 5,
			},
			searchStep(),
		},
	}
}

func mustOp(t *testing.T, spec operation.Spec) operation.Op {
	t.Helper()
	op, err := operation.Next(operation.Op{}, operation.Event{Kind: operation.Created, At: t0(), Spec: &spec})
	if err != nil {
		t.Fatalf("create op: %v", err)
	}
	return op
}

func apply(t *testing.T, op operation.Op, ev operation.Event) operation.Op {
	t.Helper()
	ev.At = t0().Add(time.Minute)
	next, err := operation.Next(op, ev)
	if err != nil {
		t.Fatalf("apply %s: %v", ev.Kind, err)
	}
	return next
}

func interruptedAt(t *testing.T, spec operation.Spec, k int) operation.Op {
	t.Helper()
	op := mustOp(t, spec)
	for j := range k {
		op = apply(t, op, operation.Event{Kind: operation.StepStarted, Index: j})
		op = apply(t, op, operation.Event{Kind: operation.StepFinished, Index: j, Outcome: operation.ResultDone})
	}
	op = apply(t, op, operation.Event{Kind: operation.StepStarted, Index: k})
	return apply(t, op, operation.Event{Kind: operation.Interrupted, Cause: operation.CauseRestart})
}

func status(code int) error {
	return fmt.Errorf("arr call: %w", &client.HTTPStatusError{Status: code, Body: "x"})
}

func notFound() error {
	return fmt.Errorf("%w: %w", client.ErrNotFound, &client.HTTPStatusError{Status: 404})
}

func states(op operation.Op) []operation.StepState {
	out := make([]operation.StepState, len(op.Results))
	for i, r := range op.Results {
		out[i] = r.State
	}
	return out
}

func wantStates(t *testing.T, op operation.Op, want ...operation.StepState) {
	t.Helper()
	got := states(op)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
}

func TestPortsAreSatisfiedByClients(t *testing.T) {
	t.Parallel()
	var _ opexec.Arr = (*client.ArrClient)(nil)
	var _ opexec.Decypharr = (*client.DecypharrClient)(nil)
}
