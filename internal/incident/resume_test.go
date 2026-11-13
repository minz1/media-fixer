package incident_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

var errBoom = &client.HTTPStatusError{Status: http.StatusInternalServerError, Body: "boom"}

var errRejected = &client.HTTPStatusError{Status: http.StatusBadRequest, Body: "no"}

type resumeArr struct {
	opexec.Arr

	mu         sync.Mutex
	calls      []string
	files      map[int]client.EpisodeFile
	movieFiles map[int]client.MovieFile
	failDelete map[int]error
	loseOnFail bool
	failSearch error
	failBlock  error
	failedHit  bool
	probeErr   error
}

func (a *resumeArr) note(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, s)
}

func (a *resumeArr) count(prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (a *resumeArr) set(f func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f()
}

func (a *resumeArr) MarkHistoryFailed(_ context.Context, id int) error {
	a.note(fmt.Sprintf("blocklist:%d", id))
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failBlock
}

func (a *resumeArr) remove(id int) error {
	a.note(fmt.Sprintf("delete:%d", id))
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.failDelete[id]
	if err == nil || a.loseOnFail {
		delete(a.files, id)
		delete(a.movieFiles, id)
	}
	return err
}

func (a *resumeArr) DeleteEpisodeFile(_ context.Context, id int) error { return a.remove(id) }

func (a *resumeArr) DeleteMovieFile(_ context.Context, id int) error { return a.remove(id) }

func (a *resumeArr) Search(_ context.Context, command string, _, _ int, _ []int) error {
	a.note("search:" + command)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failSearch
}

func (a *resumeArr) RemoveQueueItem(context.Context, int, bool, bool, bool) error { return nil }

func (a *resumeArr) GetEpisodeFiles(context.Context, int) ([]client.EpisodeFile, error) {
	a.note("read:files")
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []client.EpisodeFile
	for _, f := range a.files {
		out = append(out, f)
	}
	return out, a.probeErr
}

func (a *resumeArr) GetMovieFiles(context.Context, int) ([]client.MovieFile, error) {
	a.note("read:files")
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []client.MovieFile
	for _, f := range a.movieFiles {
		out = append(out, f)
	}
	return out, a.probeErr
}

func (a *resumeArr) GetSeries(_ context.Context, id int) (client.Series, error) {
	a.note("read:series")
	a.mu.Lock()
	defer a.mu.Unlock()
	return client.Series{ID: id}, a.probeErr
}

func (a *resumeArr) ListMovies(context.Context) ([]client.Movie, error) {
	a.note("read:movies")
	return []client.Movie{{ID: 5}}, nil
}

func (a *resumeArr) QueueEntries(context.Context) ([]client.QueueEntry, error) { return nil, nil }

func (a *resumeArr) FailedForDownload(context.Context, string, int, string, time.Time) (bool, error) {
	a.note("read:history")
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failedHit, nil
}

func (a *resumeArr) probes() int {
	return a.count("read:")
}

type resumeDecy struct {
	mu      sync.Mutex
	calls   []string
	present bool
	failAdd error
	magnets []string
}

func (d *resumeDecy) DeleteTorrent(_ context.Context, _, hash string, _ bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "tdelete:"+hash)
	return nil
}

func (d *resumeDecy) AddTorrent(_ context.Context, magnet, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "tadd")
	d.magnets = append(d.magnets, magnet)
	return d.failAdd
}

func (d *resumeDecy) TorrentByHash(context.Context, string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "tread")
	return d.present, nil
}

func (d *resumeDecy) count(prefix string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func twoFileArr() *resumeArr {
	return &resumeArr{files: map[int]client.EpisodeFile{
		10: {ID: 10, Path: "/mnt/s01e01.mkv"}, 11: {ID: 11, Path: "/mnt/s01e02.mkv"},
	}}
}

type resumeFx struct {
	*approvedFixture

	arr  *resumeArr
	decy *resumeDecy
	id   string
}

func newResumeFx(t *testing.T, arr *resumeArr, decy *resumeDecy) *resumeFx {
	t.Helper()
	fx := newApprovedFixture(t, nil, nil)
	fx.disp.Sonarr = client.NewArr("http://127.0.0.1:1", "k")
	fx.disp.Radarr = client.NewArr("http://127.0.0.1:1", "k")
	ports := opexec.Ports{Arr: func(string) opexec.Arr { return arr }}
	if decy != nil {
		ports.Decypharr = decy
		fx.disp.Decypharr = client.NewDecypharr("http://127.0.0.1:1", "t")
	}
	fx.svc.SetOpPorts(ports)
	return &resumeFx{approvedFixture: fx, arr: arr, decy: decy}
}

func (fx *resumeFx) failReplace(t *testing.T, plan client.ReplacePlan) {
	t.Helper()
	fx.id = fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams()).ID
	if err := fx.approve(t, fx.id, plan); err == nil {
		t.Fatal("approve succeeded, want a failed step")
	}
	if s := snapOf(t, fx.approvedFixture, fx.id); s.Reason != lifecycle.ReasonPlanPartial {
		t.Fatalf("incident = %s/%s, want plan_partial", s.Status, s.Reason)
	}
}

func (fx *resumeFx) resume(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	s, err := fx.db.GetIncidentSnapshotWithOp(ctx, fx.id)
	if err != nil || s.Op == nil {
		t.Fatalf("snapshot: op=%v err=%v", s.Op, err)
	}
	return fx.svc.ResumeOperation(ctx, fx.id, incident.Version{N: s.Version, Set: true}, s.Op.ID, s.Op.Version)
}

func (fx *resumeFx) op(t *testing.T) operation.Op { return incidentOp(t, fx.approvedFixture, fx.id) }

func (fx *resumeFx) state(t *testing.T) lifecycle.State {
	return snapOf(t, fx.approvedFixture, fx.id).Status
}

func TestResume_RefusesDuringShutdown(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failDelete = map[int]error{11: errRejected}
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	before := fx.op(t)
	probes := arr.probes()
	shutdownNow(t, fx.svc)

	err := fx.resume(t)

	if !errors.Is(err, card.ErrRestarting) {
		t.Fatalf("Resume during shutdown = %v, want ErrRestarting", err)
	}
	if arr.probes() != probes {
		t.Errorf("probed %d times during shutdown", arr.probes()-probes)
	}
	if after := fx.op(t); after.Version != before.Version {
		t.Errorf("op version %d -> %d, want no write", before.Version, after.Version)
	}
	snap, serr := fx.db.GetIncidentSnapshotWithOp(context.Background(), fx.id)
	if serr != nil || snap.Op == nil {
		t.Fatalf("snapshot: op=%v err=%v", snap.Op, serr)
	}
	res, cerr := fx.svc.CardHandler()(context.Background(), card.Action{
		Kind: "inc",
		Verb: "resume",
		Args: incident.ResumeArgsForTest(
			fx.id,
			incident.Version{N: snap.Version, Set: true},
			snap.Op.ID,
			snap.Op.Version,
		),
	})
	if !errors.Is(cerr, card.ErrRestarting) {
		t.Errorf("card handler = %+v %v, want ErrRestarting for Discord's ephemeral notice", res, cerr)
	}
}

func TestResume_ShutdownWhileWaitingForSlotRefuses(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failDelete = map[int]error{11: errRejected}
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	release, err := fx.svc.HoldGlobalForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	probes := arr.probes()

	done := make(chan error, 1)
	go func() { done <- fx.resume(t) }()
	time.Sleep(escalationSettle)
	shutdownNow(t, fx.svc)
	release()

	if err = <-done; !errors.Is(err, card.ErrRestarting) {
		t.Fatalf("Resume after the slot freed during shutdown = %v, want ErrRestarting", err)
	}
	if arr.probes() != probes || arr.count("search") != 0 {
		t.Errorf("Resume touched the arr during shutdown: %v", arr.calls)
	}
}

func TestResume_StaleIncidentVersionRefused(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failDelete = map[int]error{11: errRejected}
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	op := fx.op(t)
	v := fx.svc.CurrentVersion(context.Background(), fx.id)

	err := fx.svc.ResumeOperation(
		context.Background(),
		fx.id,
		incident.Version{N: v.N - 1, Set: true},
		op.ID,
		op.Version,
	)

	if !errors.Is(err, lifecycle.ErrChanged) {
		t.Fatalf("Resume with a stale incident version = %v, want %v", err, lifecycle.ErrChanged)
	}
	if arr.count("search") != 0 || fx.op(t).Version != op.Version {
		t.Errorf("a stale Resume changed something: %v", arr.calls)
	}
	res := clickResume(t, fx, "v0", "o1.1")
	if !strings.HasPrefix(res.Line, "Refused: ") {
		t.Errorf("card line = %q, want a refusal", res.Line)
	}
}

func TestResume_StaleOpVersionRefused(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failDelete = map[int]error{11: errRejected}
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	op := fx.op(t)
	v := fx.svc.CurrentVersion(context.Background(), fx.id)

	err := fx.svc.ResumeOperation(context.Background(), fx.id, v, op.ID, op.Version+1)

	if !errors.Is(err, lifecycle.ErrChanged) {
		t.Fatalf("Resume with a stale op version = %v, want %v", err, lifecycle.ErrChanged)
	}
	if arr.count("search") != 0 || arr.count("delete:11") != 1 {
		t.Errorf("a stale Resume ran steps: %v", arr.calls)
	}
	res := clickResume(t, fx, v.Token(), fmt.Sprintf("o%d.%d", op.ID, op.Version+1))
	if res.Line != "Refused: "+lifecycle.ErrChanged.Error() {
		t.Errorf("card line = %q, want the stale refusal", res.Line)
	}
}

func clickResume(t *testing.T, fx *resumeFx, v, o string) card.Result {
	t.Helper()
	res, err := fx.svc.CardHandler()(context.Background(), card.Action{
		Kind: "inc", Verb: "resume", Args: []string{fx.id, v, o},
	})
	if err != nil {
		t.Fatalf("resume click: %v", err)
	}
	return res
}

func TestResume_DoubleTapRunsOnce(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failSearch = errRejected
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	arr.set(func() { arr.failSearch = nil })
	searches := arr.count("search")
	reads := arr.probes()

	snap, err := fx.db.GetIncidentSnapshotWithOp(context.Background(), fx.id)
	if err != nil || snap.Op == nil {
		t.Fatalf("snapshot: op=%v err=%v", snap.Op, err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			errs[i] = fx.svc.ResumeOperation(context.Background(), fx.id,
				incident.Version{N: snap.Version, Set: true}, snap.Op.ID, snap.Op.Version)
		})
	}
	wg.Wait()

	failed := 0
	for _, e := range errs {
		if e != nil {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("errors = %v, want exactly one refused tap", errs)
	}
	if got := arr.count("search") - searches; got != 1 {
		t.Errorf("search sent %d times, want once", got)
	}
	if got := arr.probes() - reads; got != 1 {
		t.Errorf("probe reads = %d, want one (the loser must not probe)", got)
	}
	if fx.state(t) != lifecycle.Verifying {
		t.Errorf("incident = %s, want verifying", fx.state(t))
	}
}

func TestResume_ConflictShowsProbeNote(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failDelete = map[int]error{11: errRejected}
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	before := fx.op(t)
	arr.set(func() { arr.files[11] = client.EpisodeFile{ID: 11, Path: "/mnt/other.mkv"} })

	err := fx.resume(t)

	if err == nil || !strings.Contains(err.Error(), "file 11 changed since approval") || !incident.IsRefusal(err) {
		t.Fatalf("Resume with a conflicting probe = %v, want a refusal carrying the probe note", err)
	}
	if arr.count("delete:11") != 1 || arr.count("search") != 0 {
		t.Errorf("a refused Resume ran steps: %v", arr.calls)
	}
	if after := fx.op(t); after.Version != before.Version {
		t.Errorf("op version %d -> %d, want no write", before.Version, after.Version)
	}
	v := fx.svc.CurrentVersion(context.Background(), fx.id)
	res := clickResume(t, fx, v.Token(), fmt.Sprintf("o%d.%d", before.ID, before.Version))
	if !strings.Contains(res.Line, "file 11 changed since approval") {
		t.Errorf("card line = %q, want the probe note", res.Line)
	}
}

func TestResume_DeletedFileIDIsDoneNotDeletedAgain(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failDelete = map[int]error{10: errBoom}
	arr.loseOnFail = true
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	arr.set(func() { arr.failDelete = nil })

	if err := fx.resume(t); err != nil {
		t.Fatal(err)
	}

	if arr.count("delete:10") != 1 {
		t.Errorf("file 10 deleted %d times, want once", arr.count("delete:10"))
	}
	if arr.count("delete:11") != 1 || arr.count("search") != 1 {
		t.Errorf("calls = %v, want file 11 deleted and the search sent", arr.calls)
	}
	if fx.op(t).Status != operation.Done || fx.state(t) != lifecycle.Verifying {
		t.Errorf("op=%s incident=%s, want done and verifying", fx.op(t).Status, fx.state(t))
	}
}

func TestResume_TorrentAlreadyPresentIsNotAddedTwice(t *testing.T) {
	t.Parallel()
	decy := &resumeDecy{failAdd: errBoom}
	fx := newResumeFx(t, &resumeArr{}, decy)
	fx.id = fx.awaiting(t, agent.EscalateDeleteTorrentReadd, map[string]any{"name": "The.Boys.S01"}).ID
	plan := client.ReaddPlan{
		Name:     "The.Boys.S01",
		InfoHash: "abc",
		Category: "sonarr",
		Magnet:   "magnet:?xt=urn:btih:abc",
	}
	if err := fx.approve(t, fx.id, plan); err == nil {
		t.Fatal("approve succeeded, want the add failure")
	}
	decy.mu.Lock()
	decy.present = true
	decy.mu.Unlock()

	if err := fx.resume(t); err != nil {
		t.Fatal(err)
	}

	if decy.count("tadd") != 1 {
		t.Errorf("AddTorrent called %d times, want once", decy.count("tadd"))
	}
	if fx.op(t).Status != operation.Done || fx.state(t) != lifecycle.Verifying {
		t.Errorf("op=%s incident=%s, want done and verifying", fx.op(t).Status, fx.state(t))
	}
}

func TestResume_UnknownBlocklistIsProbedNotRepeated(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failBlock = errBoom
	fx := newResumeFx(t, arr, nil)
	plan := twoFiles()
	plan.SkipBlocklist = false
	plan.GrabsToBlocklist = []client.HistoryRecord{{ID: 5, DownloadID: "dl1", Date: time.Now().Add(-time.Hour)}}
	fx.failReplace(t, plan)
	arr.set(func() { arr.failBlock, arr.failedHit = nil, true })

	if err := fx.resume(t); err != nil {
		t.Fatal(err)
	}

	if arr.count("blocklist:5") != 1 {
		t.Errorf("blocklist sent %d times, want once", arr.count("blocklist:5"))
	}
	if arr.count("delete:10") != 1 || arr.count("delete:11") != 1 || arr.count("search") != 1 {
		t.Errorf("calls = %v, want both deletes and the search after the probe", arr.calls)
	}
}

func movieFx(t *testing.T) *resumeFx {
	t.Helper()
	arr := &resumeArr{
		movieFiles: map[int]client.MovieFile{7: {ID: 7, Path: "/mnt/movie.mkv"}},
		failDelete: map[int]error{7: errBoom}, loseOnFail: true,
	}
	fx := newResumeFx(t, arr, nil)
	plan := client.ReplacePlan{
		MediaType: client.ReplaceMediaMovie, Title: "Movie", Scope: client.ReplaceScopeSeries, MediaID: 5,
		SeasonNumber: -1, EpisodeNumber: -1, EpisodeID: -1, SkipBlocklist: true,
		Files:  []client.ReplaceFile{{ID: 7, Path: "/mnt/movie.mkv"}},
		Reason: client.ReplaceReasonWrongContent,
	}
	fx.id = fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams()).ID
	if err := fx.approve(t, fx.id, plan); err == nil {
		t.Fatal("approve succeeded, want the delete failure")
	}
	return fx
}

func TestResume_MovieUnknownDeleteStillSearches(t *testing.T) {
	t.Parallel()
	fx := movieFx(t)
	fx.arr.set(func() { fx.arr.failDelete = nil })

	if err := fx.resume(t); err != nil {
		t.Fatal(err)
	}

	if fx.arr.count("delete:7") != 1 {
		t.Errorf("movie file deleted %d times, want once", fx.arr.count("delete:7"))
	}
	if fx.arr.count("search") != 1 {
		t.Errorf("search sent %d times, want once even though the delete was unknown", fx.arr.count("search"))
	}
	if fx.state(t) != lifecycle.Verifying {
		t.Errorf("incident = %s, want verifying", fx.state(t))
	}
}

func TestS9_4_ResumeContinuesFromTheFailedStep(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failDelete = map[int]error{11: errBoom}
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	arr.set(func() { arr.failDelete = nil })

	if err := fx.resume(t); err != nil {
		t.Fatal(err)
	}

	if arr.count("delete:10") != 1 || arr.count("delete:11") != 2 || arr.count("search") != 1 {
		t.Errorf("calls = %v, want file 10 once, file 11 twice, one search", arr.calls)
	}
	if fx.op(t).Status != operation.Done || fx.state(t) != lifecycle.Verifying {
		t.Errorf("op=%s incident=%s, want done and verifying", fx.op(t).Status, fx.state(t))
	}
	rows := fx.actions(t, fx.id)
	if len(rows) != 2 || rows[1].Status != "applied" {
		t.Errorf("log rows = %s, want the failed run then an applied Resume row", describeActions(rows))
	}
}

func TestS9_6_ResumeSendsTheSearch(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failSearch = errRejected
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	arr.set(func() { arr.failSearch = nil })

	if err := fx.resume(t); err != nil {
		t.Fatal(err)
	}

	if arr.count("delete") != 2 || arr.count("search") != 2 {
		t.Errorf("calls = %v, want the deletes once and the search twice", arr.calls)
	}
	if fx.state(t) != lifecycle.Verifying {
		t.Errorf("incident = %s, want verifying", fx.state(t))
	}
}

func TestQ5a_6_ResumeReaddsFromTheStoredMagnet(t *testing.T) {
	t.Parallel()
	decy := &resumeDecy{failAdd: errRejected}
	fx := newResumeFx(t, &resumeArr{}, decy)
	fx.id = fx.awaiting(t, agent.EscalateDeleteTorrentReadd, map[string]any{"name": "The.Boys.S01"}).ID
	plan := client.ReaddPlan{
		Name:     "The.Boys.S01",
		InfoHash: "abc",
		Category: "sonarr",
		Magnet:   "magnet:?xt=urn:btih:abc",
	}
	if err := fx.approve(t, fx.id, plan); err == nil {
		t.Fatal("approve succeeded, want the add failure")
	}
	decy.mu.Lock()
	decy.failAdd = nil
	decy.mu.Unlock()

	if err := fx.resume(t); err != nil {
		t.Fatal(err)
	}

	decy.mu.Lock()
	magnets := slices.Clone(decy.magnets)
	decy.mu.Unlock()
	if len(magnets) != 2 || magnets[1] != plan.Magnet {
		t.Errorf("magnets added = %v, want the stored magnet again", magnets)
	}
	if decy.count("tdelete") != 1 {
		t.Errorf("torrent deleted %d times, want once", decy.count("tdelete"))
	}
	if fx.state(t) != lifecycle.Verifying {
		t.Errorf("incident = %s, want verifying", fx.state(t))
	}
}

func resumeButton(t *testing.T, fx *resumeFx) (card.Button, card.Update) {
	t.Helper()
	up := renderInc(t, fx.svc, fx.id)
	for _, b := range up.Buttons {
		if a, err := card.Decode(b.ID); err == nil && a.Verb == "resume" {
			return b, up
		}
	}
	t.Fatalf("no Resume button in %v", buttonVerbsOf(t, up.Buttons))
	return card.Button{}, up
}

func TestResume_ButtonIsNormalStyleWithoutConfirm(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failSearch = errRejected
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	arr.set(func() { arr.failSearch = nil })

	b, _ := resumeButton(t, fx)
	if b.Style != card.Secondary {
		t.Errorf("style = %v, want secondary", b.Style)
	}
	a, err := card.Decode(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := fx.svc.CardHandler()(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Confirm) != 0 || strings.HasPrefix(res.Line, "Refused") || res.Line == "" {
		t.Errorf("result = %+v, want it to run at once", res)
	}
	if fx.state(t) != lifecycle.Verifying {
		t.Errorf("incident = %s, want verifying after one tap", fx.state(t))
	}
}

func TestResume_ShowsNextBesideButton(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failSearch = errRejected
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())

	_, up := resumeButton(t, fx)

	want := operation.NextLine(fx.op(t))
	if want == "" {
		t.Fatal("NextLine is empty")
	}
	found := false
	for _, f := range up.Sections {
		found = found || strings.Contains(f.Value, want)
	}
	if !found {
		t.Errorf("sections = %v, want %q", up.Sections, want)
	}
}

func TestResume_KeepsRerunResolveUnlock(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failSearch = errRejected
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	lifecycletest.Lock(t, fx.db, fx.id, true)

	up := renderInc(t, fx.svc, fx.id)

	got := buttonVerbsOf(t, up.Buttons)
	for _, want := range []string{"resume", "rerun", "resolve", "unlock"} {
		if !slices.Contains(got, want) {
			t.Errorf("buttons = %v, missing %s", got, want)
		}
	}
	if slices.Contains(got, "approve") || slices.Contains(got, "preview") {
		t.Errorf("buttons = %v, want no Preview or Approve beside Resume", got)
	}
}

func TestResume_RunsWhilePaused(t *testing.T) {
	t.Parallel()
	arr := twoFileArr()
	arr.failSearch = errRejected
	fx := newResumeFx(t, arr, nil)
	fx.failReplace(t, twoFiles())
	arr.set(func() { arr.failSearch = nil })
	if err := fx.svc.SetAutonomousPaused(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	if err := fx.resume(t); err != nil {
		t.Fatalf("Resume while paused = %v, want it to run", err)
	}

	if fx.state(t) != lifecycle.Verifying || arr.count("search") != 2 {
		t.Errorf("incident=%s searches=%d, want verifying after the search", fx.state(t), arr.count("search"))
	}
}

func TestResume_ButtonIDFits(t *testing.T) {
	t.Parallel()
	id := "0123456789abcdef0123456789abcdef0123"
	args := incident.ResumeArgsForTest(id, incident.Version{N: 9999999999, Set: true}, 9999999999, 9999999999)
	if _, ok := card.Encode(card.Action{Kind: "inc", Verb: "resume", Args: args}); !ok {
		t.Errorf("Resume button id with args %v exceeds %d chars", args, card.MaxIDLen)
	}
}
