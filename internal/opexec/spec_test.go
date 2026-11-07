package opexec_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
	"github.com/minz1/mediafixer/internal/opexec"
)

type recorder struct {
	mu     sync.Mutex
	lines  []string
	failOn string
	srv    *httptest.Server
}

func newRecorder(t *testing.T, failOn string) *recorder {
	t.Helper()
	rec := &recorder{failOn: failOn}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		line := r.Method + " " + r.URL.RequestURI() + " " + string(body)
		rec.mu.Lock()
		rec.lines = append(rec.lines, line)
		rec.mu.Unlock()
		if rec.failOn != "" && strings.Contains(r.URL.Path, rec.failOn) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

func grabs() []client.HistoryRecord {
	return []client.HistoryRecord{
		{ID: 11, DownloadID: "dl11", Date: t0()},
		{ID: 12, DownloadID: "dl12", Date: t0().Add(time.Hour)},
	}
}

func files() []client.ReplaceFile {
	return []client.ReplaceFile{
		{ID: 21, Path: "/tv/a.mkv", Size: 10},
		{ID: 22, Path: "/tv/b.mkv", Size: 20},
	}
}

func plans(skip bool) map[string]*client.ReplacePlan {
	return map[string]*client.ReplacePlan{
		"movie": {
			MediaType: client.ReplaceMediaMovie, Title: "Film", MediaID: 7, SeasonNumber: -1, EpisodeNumber: -1,
			EpisodeID: -1, Files: files(), GrabsToBlocklist: grabs(), SkipBlocklist: skip,
		},
		"episode": {
			MediaType: client.ReplaceMediaTV, Title: "Show", Scope: client.ReplaceScopeEpisode, MediaID: 5,
			SeasonNumber: 1, EpisodeNumber: 2, EpisodeID: 55, Files: files(), GrabsToBlocklist: grabs(),
			SkipBlocklist: skip,
		},
		"season": {
			MediaType: client.ReplaceMediaTV, Title: "Show", Scope: client.ReplaceScopeSeason, MediaID: 5,
			SeasonNumber: 3, EpisodeNumber: -1, EpisodeID: -1, Files: files(), GrabsToBlocklist: grabs(),
			SkipBlocklist: skip,
		},
		"series": {
			MediaType: client.ReplaceMediaTV, Title: "Show", Scope: client.ReplaceScopeSeries, MediaID: 5,
			SeasonNumber: -1, EpisodeNumber: -1, EpisodeID: -1, Files: files(), GrabsToBlocklist: grabs(),
			SkipBlocklist: skip,
		},
	}
}

func execOnRecorder(t *testing.T, rec *recorder, op operation.Op) operation.Op {
	t.Helper()
	j := &journal{}
	st := &memStore{j: j, op: op}
	arr := client.NewArr(rec.srv.URL, "key")
	x := &opexec.Executor{
		Store: st, Ports: opexec.Ports{Arr: func(string) opexec.Arr { return arr }},
		Now: t0, StepTimeout: 5 * time.Second,
	}
	return x.Run(context.Background(), op)
}

func TestReplaceSpec_MatchesExecuteReplaceOrder(t *testing.T) {
	t.Parallel()
	for _, skip := range []bool{false, true} {
		for name := range plans(skip) {
			t.Run(fmt.Sprintf("%s skip=%t", name, skip), func(t *testing.T) {
				t.Parallel()
				plan := plans(skip)[name]
				oldRec := newRecorder(t, "")
				old := client.NewArr(oldRec.srv.URL, "key")
				if _, err := old.ExecuteReplace(context.Background(), plan); err != nil {
					t.Fatalf("ExecuteReplace: %v", err)
				}
				spec, err := opexec.ReplaceSpec(incident(), plan, []byte(`{}`))
				if err != nil {
					t.Fatalf("ReplaceSpec: %v", err)
				}
				newRec := newRecorder(t, "")
				got := execOnRecorder(t, newRec, mustOp(t, spec))
				if got.Status != operation.Done {
					t.Fatalf("status = %s", got.Status)
				}
				if !slices.Equal(oldRec.log(), newRec.log()) {
					t.Fatalf("calls differ\nold %q\nnew %q", oldRec.log(), newRec.log())
				}
			})
		}
	}
}

func TestReplaceSpec_RefusesOverMaxSteps(t *testing.T) {
	t.Parallel()
	plan := plans(false)["season"]
	plan.GrabsToBlocklist = nil
	plan.Files = make([]client.ReplaceFile, operation.MaxSteps())
	for i := range plan.Files {
		plan.Files[i] = client.ReplaceFile{ID: i + 1, Path: "/tv/x.mkv"}
	}
	_, err := opexec.ReplaceSpec(incident(), plan, nil)
	want := fmt.Sprintf(
		"this plan has %d steps; one approval runs at most 2000; narrow the scope", operation.MaxSteps()+1)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	plan.Files = plan.Files[1:]
	if _, err = opexec.ReplaceSpec(incident(), plan, nil); err != nil {
		t.Fatalf("2000 steps refused: %v", err)
	}
}

func TestReaddSpec_RefusesBlankMagnet(t *testing.T) {
	t.Parallel()
	for _, magnet := range []string{"", "   "} {
		plan := &client.ReaddPlan{Name: "Show", InfoHash: "abc", Category: "tv", Magnet: magnet}
		if _, err := opexec.ReaddSpec(incident(), plan, nil); !errors.Is(err, client.ErrNoMagnet) {
			t.Fatalf("magnet %q: err = %v", magnet, err)
		}
	}
	plan := &client.ReaddPlan{Name: "Show", InfoHash: "abc", Category: "tv", Magnet: "magnet:?xt=abc"}
	spec, err := opexec.ReaddSpec(incident(), plan, []byte(`{}`))
	if err != nil {
		t.Fatalf("ReaddSpec: %v", err)
	}
	if spec.Kind != operation.KindTorrentReadd || len(spec.Steps) != 2 || spec.Steps[1].Magnet != plan.Magnet {
		t.Fatalf("spec = %+v", spec)
	}
	mustOp(t, spec)
}
