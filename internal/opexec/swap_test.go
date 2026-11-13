package opexec_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

const (
	swapOld  = "/tv/Show/S01E06.old.mkv"
	swapNew  = "/data/downloads/sonarr/Show.S01E06/Show.S01E06.mkv"
	swapSize = int64(900)
)

func swapSteps(skip bool) []operation.Step {
	steps := []operation.Step{
		{
			Kind: operation.StepImport, App: "sonarr", Path: swapNew, Size: swapSize, FileID: 41, MediaID: 7,
			EpisodeIDs: []int{6}, Season: 1, Episode: 6,
		},
		{
			Kind: operation.StepJellyfinNotify, App: "jellyfin", Path: "/media/tv/Show/S01E06.old.mkv",
			Name: "jf-series", Season: 1, Episode: 6,
		},
	}
	if !skip {
		steps = append(steps, operation.Step{
			Kind: operation.StepBlocklist, App: "sonarr", HistoryID: 3, DownloadID: "dl3", GrabbedAt: t0(),
			Name: "Show.S01E06", MediaID: 7, Season: 1, Episode: 6,
		})
	}
	return steps
}

func swapPlan(skip bool) *client.SwapPlan {
	return &client.SwapPlan{
		Title: "Show S01E06", SeriesID: 7, Season: 1, Episode: 6, EpisodeID: 6,
		Blocklist: client.SwapBlocklist{Skip: skip}, Steps: swapSteps(skip),
	}
}

func swapOp(t *testing.T) operation.Op {
	t.Helper()
	spec, err := opexec.SwapSpec(incident(), swapPlan(false), []byte(`{}`))
	if err != nil {
		t.Fatalf("SwapSpec: %v", err)
	}
	return mustOp(t, spec)
}

func candidate() client.ImportCandidate {
	return client.ImportCandidate{
		Path: swapNew, Size: swapSize, SeriesID: 7, SeasonNumber: 1, DownloadID: "dl-x",
		Episodes: []client.CandidateEpisode{{ID: 6, SeasonNumber: 1, EpisodeNumber: 6}},
	}
}

type jelly struct {
	j     *journal
	err   error
	paths []string
}

func (f *jelly) MediaUpdated(_ context.Context, paths []string) error {
	f.j.add("call:notify")
	f.paths = paths
	return f.err
}

func swapRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(swapOp(t))
	r.x.PollInterval = time.Millisecond
	r.arr.episodes = []client.Episode{{ID: 6, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 6, EpisodeFileID: 41}}
	r.arr.epFiles = []client.EpisodeFile{{ID: 41, Path: swapOld, Size: 500}}
	r.arr.cands = []client.ImportCandidate{candidate()}
	r.arr.cmdStatus = "completed"
	r.x.Ports.Jellyfin = &jelly{j: r.j}
	return r
}

func setFile(a *fakeArr, id int, size int64) {
	a.episodes = []client.Episode{{ID: 6, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 6, EpisodeFileID: id}}
	a.epFiles = []client.EpisodeFile{{ID: id, Path: swapNew, Size: size}}
}

func runImport(t *testing.T, r *rig) operation.Op {
	t.Helper()
	return r.x.Run(context.Background(), r.store.op)
}

func wantNothingPosted(t *testing.T, r *rig, note string) {
	t.Helper()
	if r.j.has("call:import") {
		t.Fatalf("a ManualImport was posted: %v", r.j.all())
	}
	got := r.store.op.Results[0]
	if got.State != operation.ResultFailed || !strings.Contains(got.Err, note) {
		t.Fatalf("import result = %s %q, want failed containing %q", got.State, got.Err, note)
	}
}

func TestD5_SwapSpec(t *testing.T) {
	t.Parallel()
	for _, skip := range []bool{false, true} {
		spec, err := opexec.SwapSpec(incident(), swapPlan(skip), []byte(`{}`))
		if err != nil {
			t.Fatalf("SwapSpec: %v", err)
		}
		want := 3
		if skip {
			want = 2
		}
		if spec.Kind != operation.KindSwapRelease || len(spec.Steps) != want {
			t.Fatalf("skip=%t: kind %s, %d steps, want %d", skip, spec.Kind, len(spec.Steps), want)
		}
		n := spec.Steps[1]
		wantPath := "/media/tv/Show/S01E06.old.mkv"
		if n.Kind != operation.StepJellyfinNotify || n.Path != wantPath || n.Name != "jf-series" {
			t.Fatalf("notify step = %+v", n)
		}
		mustOp(t, spec)
	}
	if _, err := opexec.SwapSpec(incident(), &client.SwapPlan{}, nil); err == nil {
		t.Fatal("a plan without steps must be refused")
	}
}

func TestD5_ImportFreshReadChanged(t *testing.T) {
	t.Parallel()
	cases := map[string]func(c *client.ImportCandidate) []client.ImportCandidate{
		"size": func(c *client.ImportCandidate) []client.ImportCandidate {
			c.Size++
			return []client.ImportCandidate{*c}
		},
		"gone": func(*client.ImportCandidate) []client.ImportCandidate { return nil },
		"path": func(c *client.ImportCandidate) []client.ImportCandidate {
			c.Path += "x"
			return []client.ImportCandidate{*c}
		},
		"episode": func(c *client.ImportCandidate) []client.ImportCandidate {
			c.Episodes[0].ID = 9
			return []client.ImportCandidate{*c}
		},
		"two": func(c *client.ImportCandidate) []client.ImportCandidate {
			c.Episodes = append(c.Episodes, client.CandidateEpisode{ID: 7})
			return []client.ImportCandidate{*c}
		},
		"series": func(c *client.ImportCandidate) []client.ImportCandidate {
			c.SeriesID = 8
			return []client.ImportCandidate{*c}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := swapRig(t)
			c := candidate()
			r.arr.cands = mut(&c)
			runImport(t, r)
			wantNothingPosted(t, r, "the candidate changed; preview again")
		})
	}
}

func TestD5_ImportBrokenFileGone(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	setFile(r.arr, 55, 1)
	runImport(t, r)
	wantNothingPosted(t, r, "S01E06 has a different file since approval")
}

func TestD5_ImportQueueChanged(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.queue = []client.QueueEntry{{ID: 1, DownloadID: "Y", SeriesID: 7, EpisodeID: 6}}
	runImport(t, r)
	wantNothingPosted(t, r, "queued")
}

func TestD5_ImportPending(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.pending = true
	runImport(t, r)
	wantNothingPosted(t, r, "already being imported")
}

func TestD5_ImportPreReadErrorsFailNothingPosted(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(a *fakeArr){
		"episodes": func(a *fakeArr) { a.episodesErr = errors.New("boom") },
		"queue":    func(a *fakeArr) { a.queueErr = status(500) },
		"pending":  func(a *fakeArr) { a.pendingErr = errors.New("eof") },
		"folder":   func(a *fakeArr) { a.candsErr = status(502) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := swapRig(t)
			set(r.arr)
			runImport(t, r)
			wantNothingPosted(t, r, "")
		})
	}
}

func TestD5_ImportCompletedOldFile(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	runImport(t, r)
	got := r.store.op.Results[0]
	if got.State != operation.ResultFailed || !r.j.has("call:import") {
		t.Fatalf("result = %s %q, journal %v", got.State, got.Err, r.j.all())
	}
}

func TestD5_ImportCompletedNoFile(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.afterImport = func(a *fakeArr) {
		a.episodes = []client.Episode{{ID: 6, SeriesID: 7, SeasonNumber: 1, EpisodeNumber: 6}}
		a.epFiles = nil
	}
	op := runImport(t, r)
	got := op.Results[0]
	if got.State != operation.ResultUnknown || !strings.HasPrefix(got.Note, "fileless:") {
		t.Fatalf("result = %s %q", got.State, got.Note)
	}
	if op.Status == operation.Done {
		t.Fatalf("op status = %s", op.Status)
	}
}

func TestD5_ImportDone(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.afterImport = func(a *fakeArr) { setFile(a, 60, swapSize) }
	op := runImport(t, r)
	got := op.Results[0]
	if got.State != operation.ResultDone || got.Note != "imported as "+swapNew {
		t.Fatalf("result = %s %q", got.State, got.Note)
	}
	wantStates(t, op, operation.ResultDone, operation.ResultDone, operation.ResultDone)
	if !r.j.has("read:folder:/data/downloads/sonarr/Show.S01E06") {
		t.Fatalf("fresh read not of the candidate folder: %v", r.j.all())
	}
}

func TestD5_ImportDoneNeedsSize(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.afterImport = func(a *fakeArr) { setFile(a, 60, swapSize+1) }
	op := runImport(t, r)
	if got := op.Results[0].State; got != operation.ResultUnknown {
		t.Fatalf("a different file of another size = %s, want unknown", got)
	}
}

func TestD5_ImportPostErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want operation.StepState
	}{"4xx": {status(400), operation.ResultFailed}, "5xx": {status(500), operation.ResultUnknown}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := swapRig(t)
			r.arr.postErr = tc.err
			runImport(t, r)
			if got := r.store.op.Results[0].State; got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
			n := 0
			for _, l := range r.j.all() {
				if l == "call:import" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("posted %d times", n)
			}
		})
	}
}

func TestD5_ImportPollTimeout(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.x.StepTimeout = 50 * time.Millisecond
	r.arr.cmdStatus = "started"
	runImport(t, r)
	if got := r.store.op.Results[0].State; got != operation.ResultUnknown {
		t.Fatalf("state = %s, want unknown", got)
	}
}

func TestD5_ImportPollErrorAfterPostIsUnknown(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.cmdErr = notFound()
	runImport(t, r)
	if got := r.store.op.Results[0].State; got != operation.ResultUnknown {
		t.Fatalf("state = %s, want unknown", got)
	}
}

func TestD5_NotifyStatuses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want operation.StepState
	}{
		"204":       {nil, operation.ResultDone},
		"4xx":       {status(401), operation.ResultFailed},
		"5xx":       {status(502), operation.ResultUnknown},
		"transport": {errors.New("connection reset"), operation.ResultUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := swapRig(t)
			r.arr.afterImport = func(a *fakeArr) { setFile(a, 60, swapSize) }
			jf := &jelly{j: r.j, err: tc.err}
			r.x.Ports.Jellyfin = jf
			op := runImport(t, r)
			if got := op.Results[1].State; got != tc.want {
				t.Fatalf("notify state = %s, want %s", got, tc.want)
			}
			if len(jf.paths) != 1 || jf.paths[0] != "/media/tv/Show" {
				t.Fatalf("notified %v, want the old file's folder", jf.paths)
			}
		})
	}
}

func TestD5_NotifyWithoutPortFails(t *testing.T) {
	t.Parallel()
	r := swapRig(t)
	r.arr.afterImport = func(a *fakeArr) { setFile(a, 60, swapSize) }
	r.x.Ports.Jellyfin = nil
	op := runImport(t, r)
	if got := op.Results[1].State; got != operation.ResultFailed {
		t.Fatalf("notify state = %s", got)
	}
}

func TestD5_ImportEntryHasNoDownloadID(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posted string
	fileID := 41
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && p == "/api/v3/command":
			b, _ := io.ReadAll(r.Body)
			posted = string(b)
			fileID = 60
			_, _ = w.Write([]byte(`{"id":9}`))
		case p == "/api/v3/command/9":
			_, _ = w.Write([]byte(`{"status":"completed"}`))
		case p == "/api/v3/command":
			_, _ = w.Write([]byte(`[]`))
		case p == "/api/v3/manualimport":
			if r.URL.Query().Has("seriesId") {
				t.Error("the folder read must not send seriesId")
			}
			_, _ = w.Write([]byte(`[{"path":"` + swapNew + `","size":900,"downloadId":"dl-x","seriesId":7,` +
				`"seasonNumber":1,"episodes":[{"id":6,"seasonNumber":1,"episodeNumber":6}]}]`))
		case p == "/api/v3/queue":
			_, _ = w.Write([]byte(`{"totalRecords":0,"records":[]}`))
		case p == "/api/v3/episode":
			eps := []client.Episode{{ID: 6, SeriesID: 7, SeasonNumber: 1, EpisodeFileID: fileID}}
			_ = json.NewEncoder(w).Encode(eps)
		case p == "/api/v3/episodefile":
			_ = json.NewEncoder(w).Encode([]client.EpisodeFile{{ID: fileID, Path: swapNew, Size: swapSize}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	arr := client.NewArr(srv.URL, "k")
	op := mustOp(t, mustSwapSpec(t))
	x := &opexec.Executor{
		Store: &memStore{j: &journal{}, op: op}, Ports: opexec.Ports{Arr: func(string) opexec.Arr { return arr }},
		Now: t0, StepTimeout: 5 * time.Second, PollInterval: time.Millisecond,
	}
	got := x.Run(context.Background(), op)
	if got.Results[0].State != operation.ResultDone {
		t.Fatalf("import = %s %q %q", got.Results[0].State, got.Results[0].Err, got.Results[0].Note)
	}
	mu.Lock()
	defer mu.Unlock()
	var body struct {
		ImportMode string `json:"importMode"`
		Files      []struct {
			DownloadID string `json:"downloadId"`
			Path       string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(posted), &body); err != nil {
		t.Fatalf("posted %q: %v", posted, err)
	}
	if body.ImportMode != "move" || len(body.Files) != 1 ||
		body.Files[0].DownloadID != "" || body.Files[0].Path != swapNew {
		t.Fatalf("posted body = %s", posted)
	}
}

func mustSwapSpec(t *testing.T) operation.Spec {
	t.Helper()
	spec, err := opexec.SwapSpec(incident(), swapPlan(true), []byte(`{}`))
	if err != nil {
		t.Fatalf("SwapSpec: %v", err)
	}
	return spec
}
