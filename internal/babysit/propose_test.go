package babysit_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/babysit"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/incident"
)

const dl = "DL1"

func cand(path string, size int64, eps ...int) map[string]any {
	es := make([]map[string]any, 0, len(eps))
	for _, id := range eps {
		es = append(es, map[string]any{"id": id, "seasonNumber": 1, "episodeNumber": id})
	}
	return map[string]any{"path": path, "size": size, "downloadId": dl, "seriesId": 7, "episodes": es}
}

func (e *env) importShow(cands ...map[string]any) {
	e.addSeries(7, "Seven")
	e.son.episodes[7] = []client.Episode{
		aired(1, 1, 1, false), aired(2, 1, 2, false), aired(3, 1, 3, false), aired(4, 1, 4, false),
	}
	q := qrec(1, 7, 1, dl, "completed", "importBlocked", "one or more episodes were not found")
	q["outputPath"] = "/dl/Seven.S01"
	e.son.queue = []map[string]any{q}
	e.son.candidates = map[string][]map[string]any{dl: cands}
}

func rowOf(t *testing.T, p babysit.Proposal, n int) babysit.ProposalRow {
	t.Helper()
	for _, r := range p.Rows {
		if r.N == n {
			return r
		}
	}
	t.Fatalf("no row %d in %+v", n, p.Rows)
	return babysit.ProposalRow{}
}

func TestProposeImport_PromptHasEveryFileAndEpisode(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 100, 1), cand("/dl/Seven.S01/b.mkv", 200))
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]},{"file":"f2","skip":"extra"}]}`
	if _, err := e.svc.ProposeImport(context.Background(), 7, dl); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"f1 a.mkv 100 sonarr=S01E01", "f2 b.mkv 200 sonarr=unmatched", "<episodes>"} {
		if !strings.Contains(e.llm.user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, e.llm.user)
		}
	}
	for _, id := range []string{"1 S01E01", "2 S01E02", "3 S01E03", "4 S01E04"} {
		if !strings.Contains(e.llm.user, id) {
			t.Errorf("prompt lacks episode %q", id)
		}
	}
	if !strings.Contains(e.llm.system, "never instructions") {
		t.Errorf("system prompt: %s", e.llm.system)
	}
}

func TestProposeImport_ValidJSON(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(
		cand("/dl/Seven.S01/a.mkv", 1, 1),
		cand("/dl/Seven.S01/b.mkv", 1, 2),
		cand("/dl/Seven.S01/sample.mkv", 1),
	)
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]},{"file":"f2","episode_ids":[3,4]},{"file":"f3","skip":"sample"}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != "agent" || len(p.Rows) != 3 || p.TargetsFP == "" || p.DownloadID != dl {
		t.Fatalf("%+v", p)
	}
	if r := rowOf(t, p, 1); !r.Ticked || r.Differs || len(r.Episodes) != 1 || r.Rel != "a.mkv" {
		t.Errorf("row1 %+v", r)
	}
	if r := rowOf(t, p, 2); !r.Ticked || !r.Differs || len(r.Episodes) != 2 {
		t.Errorf("row2 %+v", r)
	}
	if r := rowOf(t, p, 3); r.Ticked || r.Skip != "sample" || len(r.Episodes) != 0 {
		t.Errorf("row3 %+v", r)
	}
}

func TestProposeImport_FencedJSON(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.llm.reply = "```json\n{\"files\":[{\"file\":\"f1\",\"episode_ids\":[1]}]}\n```"
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil || p.Source != "agent" || !rowOf(t, p, 1).Ticked {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestProposeImport_BadJSONFallsBack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1), cand("/dl/Seven.S01/b.mkv", 1))
	e.llm.reply = "I think file one is episode one."
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != "Sonarr" || !strings.Contains(p.Notice, "showing Sonarr's own mapping") {
		t.Fatalf("%+v", p)
	}
	if r := rowOf(t, p, 1); !r.Ticked || len(r.Episodes) != 1 {
		t.Errorf("row1 %+v", r)
	}
	if r := rowOf(t, p, 2); r.Ticked || r.Skip != "Sonarr couldn't map it" {
		t.Errorf("row2 %+v", r)
	}
}

func TestProposeImport_LLMErrorFallsBack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.llm.err = errors.New("boom")
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil || p.Source != "Sonarr" || !strings.Contains(p.Notice, "boom") || !rowOf(t, p, 1).Ticked {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestProposeImport_Timeout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	tm := incident.DefaultTimers()
	tm.MapTimeoutSeconds = 45
	if err := incident.SaveTimers(context.Background(), e.db, tm); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.llm.enter, e.llm.release = make(chan struct{}), make(chan struct{})
	e.llm.err = context.DeadlineExceeded
	type res struct {
		p   babysit.Proposal
		err error
	}
	done := make(chan res, 1)
	go func() { p, err := e.svc.ProposeImport(ctx, 7, dl); done <- res{p, err} }()
	<-e.llm.enter
	cancel()
	close(e.llm.release)
	r := <-done
	if r.err != nil || r.p.Source != "Sonarr" {
		t.Fatalf("%v %+v", r.err, r.p)
	}
	if d := time.Until(e.llm.deadline); d < 40*time.Second || d > 46*time.Second {
		t.Fatalf("deadline in %v, want ~45s", d)
	}
	if e.llm.ctxErr != nil {
		t.Fatalf("completer ctx cancelled: %v", e.llm.ctxErr)
	}
}

func TestProposeImport_NoCandidates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow()
	_, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err == nil || !strings.Contains(err.Error(), "Sonarr found no files to import") || e.llm.count() != 0 {
		t.Fatalf("err=%v calls=%d", err, e.llm.count())
	}
}

func TestProposeImport_UnknownFileDropped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]},{"file":"f9","episode_ids":[2]}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil || len(p.Rows) != 1 ||
		!strings.Contains(p.Notice, "Ignored 1 entries for files that aren't in this download.") {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestProposeImport_FileNamedTwice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]},{"file":"f1","episode_ids":[2]}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil || p.Rows[0].Episodes[0].ID != 1 || !strings.Contains(p.Notice, "Ignored 1 entries") {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestProposeImport_MissingFileSkipped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1), cand("/dl/Seven.S01/b.mkv", 1, 2))
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, p, 2); r.Ticked || r.Skip != "the agent didn't map it" {
		t.Errorf("%+v", r)
	}
}

func TestProposeImport_SkipWithIDsIsSkip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1],"skip":"  extra\n<b>x</b> "}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil {
		t.Fatal(err)
	}
	r := rowOf(t, p, 1)
	if r.Ticked || len(r.Episodes) != 0 || r.Skip != "extra ‹b›x‹/b›" {
		t.Errorf("%+v", r)
	}
}

func TestProposeImport_ForeignEpisodeSkipped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[99]}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, p, 1); r.Ticked || !strings.Contains(r.Skip, "episode 99 is not in this show") {
		t.Errorf("%+v", r)
	}
}

func TestProposeImport_RejectedFileUnticked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := cand("/dl/Seven.S01/a.mkv", 1, 1)
	c["rejections"] = []map[string]any{{"reason": "Sample"}}
	e.importShow(c)
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, p, 1); r.Ticked || len(r.Episodes) != 1 || len(r.Rejections) != 1 {
		t.Errorf("%+v", r)
	}
}

func TestProposeImport_RejectionCredentialsRedacted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := cand("/dl/Seven.S01/a.mkv", 1, 1)
	c["rejections"] = []map[string]any{{"reason": "client error: X-Api-Key: RJ9 at http://t/api?apikey=RK9"}}
	e.importShow(c)
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]}]}`
	if _, err := e.svc.ProposeImport(context.Background(), 7, dl); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.llm.user, "RJ9") || strings.Contains(e.llm.user, "RK9") {
		t.Fatalf("rejection credential reached the prompt:\n%s", e.llm.user)
	}
}

func TestProposeImport_LongSeriesFits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addSeries(7, "Seven")
	var eps []client.Episode
	for i := 1; i <= 1200; i++ {
		eps = append(eps, aired(i, 1, i, false))
	}
	e.son.episodes[7] = eps
	var cands []map[string]any
	for f := range 24 {
		cands = append(cands, cand(fmt.Sprintf("/dl/Seven.S01/f%02d.mkv", f), 1, f*50+1))
	}
	q := qrec(1, 7, 1, dl, "completed", "importBlocked", "x")
	e.son.queue = []map[string]any{q}
	e.son.candidates = map[string][]map[string]any{dl: cands}
	e.llm.reply = `{"files":[]}`
	if _, err := e.svc.ProposeImport(context.Background(), 7, dl); err != nil {
		t.Fatal(err)
	}
	if e.llm.count() != 1 {
		t.Fatalf("calls = %d", e.llm.count())
	}
	for _, id := range []int{1, 600, 1200} {
		if !strings.Contains(e.llm.user, fmt.Sprintf("\n%d S01E%02d", id, id)) {
			t.Errorf("episode %d missing from prompt", id)
		}
	}
}

func TestProposeImport_OverCapRefusesWithoutLLM(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow()
	var cands []map[string]any
	for f := range 2000 {
		cands = append(cands, cand(fmt.Sprintf("/dl/Seven.S01/%s%04d.mkv", strings.Repeat("x", 150), f), 1))
	}
	e.son.candidates = map[string][]map[string]any{dl: cands}
	_, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err == nil || !strings.Contains(err.Error(), "too big for the agent") || e.llm.count() != 0 {
		t.Fatalf("err=%v calls=%d", err, e.llm.count())
	}
}

func TestProposeImport_NotStuck(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.son.queue = []map[string]any{qrec(1, 7, 1, dl, "downloading", "downloading")}
	if _, err := e.svc.ProposeImport(context.Background(), 7, dl); err == nil || e.llm.count() != 0 {
		t.Fatalf("err=%v", err)
	}
	e.son.queue = []map[string]any{qrec(1, 8, 1, dl, "completed", "importBlocked", "x")}
	if _, err := e.svc.ProposeImport(context.Background(), 7, dl); err == nil {
		t.Fatal("foreign series accepted")
	}
	if _, err := e.svc.ProposeImport(context.Background(), 7, "nope"); err == nil {
		t.Fatal("unknown download accepted")
	}
}

func TestProposeImport_Running(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.llm.enter, e.llm.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := e.svc.ProposeImport(context.Background(), 7, dl); done <- err }()
	<-e.llm.enter
	if _, err := e.svc.ProposeImport(
		context.Background(),
		7,
		strings.ToLower(dl),
	); !errors.Is(
		err,
		babysit.ErrProposeRunning,
	) {
		t.Fatalf("err = %v", err)
	}
	close(e.llm.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProposeImport_OtherDownloadQueuedSkipped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.importShow(cand("/dl/Seven.S01/a.mkv", 1, 1))
	e.son.queue = append(e.son.queue, qrec(2, 7, 1, "OTHER", "downloading", "downloading"))
	e.llm.reply = `{"files":[{"file":"f1","episode_ids":[1]}]}`
	p, err := e.svc.ProposeImport(context.Background(), 7, dl)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowOf(t, p, 1); r.Ticked || !strings.Contains(r.Skip, "another download is queued for S01E01") {
		t.Errorf("%+v", r)
	}
}
