package stuckread_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
	"github.com/minz1/mediafixer/internal/stuckread"
)

type blockingAgent struct{ incident.AgentRunner }

func (blockingAgent) Run(
	ctx context.Context, _ *db.Incident, _ []openai.ChatCompletionMessage,
) (*agent.DiagnosticResult, []openai.ChatCompletionMessage, error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func (blockingAgent) ScanRunning(context.Context) bool { return false }

type recordingNotifier struct {
	mu     sync.Mutex
	alerts int
}

func (n *recordingNotifier) NotifyOwner(_ context.Context, c card.Card) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c.Tone == card.Alert {
		n.alerts++
	}
	return nil
}

func (*recordingNotifier) NotifyUser(context.Context, string, card.Card) error { return nil }

func (n *recordingNotifier) alertCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.alerts
}

type feed struct {
	readers []mediaagentapi.StuckReader
	err     error
}

func (f *feed) StuckReads(context.Context) (*mediaagentapi.StuckReadsResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &mediaagentapi.StuckReadsResult{Readers: f.readers}, nil
}

type harness struct {
	t     *testing.T
	w     *stuckread.Watcher
	feed  *feed
	svc   *incident.Service
	db    *db.DB
	path  string
	notif *recordingNotifier
	logs  *bytes.Buffer
	clock time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	h := &harness{
		t: t, feed: &feed{}, db: database, path: f.Name(), notif: &recordingNotifier{}, logs: &bytes.Buffer{},
		clock: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
	h.svc = incident.NewService(
		t.Context(), database, nil, blockingAgent{}, nil, nil, h.notif, slog.New(slog.DiscardHandler),
	)
	h.w = h.newWatcher()
	return h
}

func (h *harness) newWatcher() *stuckread.Watcher {
	w := stuckread.New(h.feed, h.svc, h.db, slog.New(slog.NewTextHandler(h.logs, nil)))
	w.SetNowForTest(func() time.Time { return h.clock })
	return w
}

func (h *harness) poll(readers ...mediaagentapi.StuckReader) {
	h.t.Helper()
	h.clock = h.clock.Add(time.Minute)
	h.feed.readers, h.feed.err = readers, nil
	h.w.Poll(h.t.Context())
}

func (h *harness) failPoll() {
	h.t.Helper()
	h.clock = h.clock.Add(time.Minute)
	h.feed.readers, h.feed.err = nil, errors.New("scan timeout")
	h.w.Poll(h.t.Context())
}

func (h *harness) incidents() []*db.Incident {
	h.t.Helper()
	list, err := h.db.ListIncidents(h.t.Context(), "", 100, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func (h *harness) setStatus(id string, s db.IncidentStatus) {
	h.t.Helper()
	if err := h.db.UpdateIncidentStatus(h.t.Context(), id, s); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) status(id string) db.IncidentStatus {
	h.t.Helper()
	inc, err := h.db.GetIncident(h.t.Context(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return inc.Status
}

func (h *harness) count(level string) int {
	return strings.Count(h.logs.String(), "level="+level)
}

func rd(title string, tid int, switches uint64) mediaagentapi.StuckReader {
	return mediaagentapi.StuckReader{
		PID: 100, TID: tid, Start: 1, Comm: "ffprobe", Switches: switches,
		Path: "/mnt/decypharr/__all__/" + title + "/e1.mkv", Via: "syscall",
	}
}

func three(title string, switches uint64) []mediaagentapi.StuckReader {
	return []mediaagentapi.StuckReader{rd(title, 1, switches), rd(title, 2, switches), rd(title, 3, switches)}
}

func TestProgressIsNotStuck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := range 10 {
		h.poll(three("T", uint64(i))...)
	}
	if n := len(h.incidents()); n != 0 {
		t.Fatalf("incidents = %d, want 0", n)
	}
}

func TestThreeStuckOpensOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for range 6 {
		h.poll(three("T", 7)...)
	}
	list := h.incidents()
	if len(list) != 1 {
		t.Fatalf("incidents = %d, want 1", len(list))
	}
	inc := list[0]
	if inc.What != "stuck_read" || inc.Source != "media-agent" || inc.ReportedBy != "media-agent" || inc.Title != "T" {
		t.Errorf("unexpected incident: %+v", inc)
	}
	for _, want := range []string{"/mnt/decypharr/__all__/T/e1.mkv", "tid=1", "tid=2", "tid=3", "pid=100", "ffprobe"} {
		if !strings.Contains(inc.Details, want) {
			t.Errorf("details missing %q: %s", want, inc.Details)
		}
	}
	reporters, err := h.db.ListReporters(t.Context(), inc.ID)
	if err != nil || len(reporters) != 1 {
		t.Errorf("reporters = %v, %v; want one", reporters, err)
	}
}

func TestTwoFilesOneTorrent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, b := rd("T", 1, 5), rd("T", 2, 5)
	b.Path = "/mnt/decypharr/__all__/T/e2.mkv"
	c, d := rd("T", 3, 5), rd("T", 4, 5)
	d.Path = b.Path
	for range 3 {
		h.poll(a, b, c, d)
	}
	if n := len(h.incidents()); n != 1 {
		t.Fatalf("incidents = %d, want 1", n)
	}
}

func TestOneTaskNeedsMinutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for range 6 {
		h.poll(rd("T", 1, 5))
	}
	if n := len(h.incidents()); n != 0 {
		t.Fatalf("incidents after 4 minutes stuck = %d, want 0", n)
	}
	h.poll(rd("T", 1, 5))
	if n := len(h.incidents()); n != 1 {
		t.Fatalf("incidents after 5 minutes stuck = %d, want 1", n)
	}
}

func TestRestartNoDuplicate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for range 3 {
		h.poll(three("T", 7)...)
	}
	h.w = h.newWatcher()
	for range 4 {
		h.poll(three("T", 7)...)
	}
	if n := len(h.incidents()); n != 1 {
		t.Fatalf("incidents = %d, want 1", n)
	}
}

func openOne(t *testing.T, h *harness) *db.Incident {
	t.Helper()
	for range 3 {
		h.poll(three("T", 7)...)
	}
	list := h.incidents()
	if len(list) != 1 {
		t.Fatalf("incidents = %d, want 1", len(list))
	}
	return list[0]
}

func TestEscalatedNotRecreated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	inc := openOne(t, h)
	h.setStatus(inc.ID, db.StatusManualTestNeeded)
	for range 4 {
		h.poll(three("T", 7)...)
	}
	if n := len(h.incidents()); n != 1 {
		t.Fatalf("incidents = %d, want 1", n)
	}
	if got := h.status(inc.ID); got != db.StatusManualTestNeeded {
		t.Fatalf("status = %s, want manual_test_needed", got)
	}
}

func TestAgentFixedRerunOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	inc := openOne(t, h)
	h.setStatus(inc.ID, db.StatusAgentFixed)
	h.poll(three("T", 7)...)
	if got := h.status(inc.ID); got != db.StatusReopened {
		t.Fatalf("status = %s, want reopened", got)
	}
	h.setStatus(inc.ID, db.StatusAgentFixed)
	for range 3 {
		h.poll(three("T", 7)...)
	}
	if got := h.status(inc.ID); got != db.StatusAgentFixed {
		t.Fatalf("status = %s, want agent_fixed (no second rerun)", got)
	}
	if n := h.count("WARN"); n != 1 {
		t.Fatalf("warn lines = %d, want 1:\n%s", n, h.logs)
	}
}

func TestRerunErrorKeepsOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	inc := openOne(t, h)
	h.setStatus(inc.ID, db.StatusAgentFixed)
	raw, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(t.Context(), `CREATE TRIGGER no_reopen BEFORE UPDATE OF status ON incidents
		WHEN NEW.status = 'reopened' BEGIN SELECT RAISE(ABORT, 'db busy'); END`); err != nil {
		t.Fatal(err)
	}
	h.poll(three("T", 7)...)
	if got := h.status(inc.ID); got != db.StatusAgentFixed {
		t.Fatalf("status = %s, want agent_fixed after the failed rerun", got)
	}
	if _, err = raw.ExecContext(t.Context(), `DROP TRIGGER no_reopen`); err != nil {
		t.Fatal(err)
	}
	h.poll(three("T", 7)...)
	if got := h.status(inc.ID); got != db.StatusReopened {
		t.Fatalf("status = %s, want reopened: a failed rerun must not use up the once", got)
	}
}

func TestRerunGatedByOpenCount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i := range incident.SystematicIncidentThreshold {
		if _, err := h.svc.Handle(t.Context(), &incident.Report{
			Source: "discord", ReportedBy: "u", What: "other", Title: fmt.Sprintf("other-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	fixed := &db.Incident{Status: db.StatusAgentFixed, Source: "media-agent", ReportedBy: "media-agent",
		What: "stuck_read", Title: "T"}
	if err := h.db.CreateIncident(t.Context(), fixed); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		h.poll(three("T", 7)...)
	}
	if got := h.status(fixed.ID); got != db.StatusAgentFixed {
		t.Fatalf("status = %s, want agent_fixed while the open count is at the threshold", got)
	}
	other := h.incidents()
	for _, inc := range other {
		if inc.Title == "other-0" {
			h.setStatus(inc.ID, db.StatusResolved)
		}
	}
	h.poll(three("T", 7)...)
	if got := h.status(fixed.ID); got != db.StatusReopened {
		t.Fatalf("status = %s, want reopened once the count dropped", got)
	}
}

func TestWedgeStopsAtSystematicLock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wedge := func() []mediaagentapi.StuckReader {
		var all []mediaagentapi.StuckReader
		for i := range 8 {
			all = append(all, three(fmt.Sprintf("T%d", i), 7)...)
		}
		return all
	}
	h.poll(wedge()...)
	h.poll(wedge()...)
	list := h.incidents()
	blocked := 0
	for _, inc := range list {
		if inc.Status == db.StatusBlocked {
			blocked++
		}
	}
	if len(list) != 8 || blocked != 3 || h.notif.alertCount() != 1 {
		t.Fatalf("incidents=%d blocked=%d alerts=%d, want 8/3/1", len(list), blocked, h.notif.alertCount())
	}
	h.poll(wedge()...)
	h.poll(wedge()...)
	if len(h.incidents()) != 8 || h.notif.alertCount() != 1 {
		t.Fatalf("later polls changed state: incidents=%d alerts=%d", len(h.incidents()), h.notif.alertCount())
	}
	for _, inc := range h.incidents() {
		if inc.Status == db.StatusReopened {
			t.Fatalf("incident %s was rerun", inc.Title)
		}
	}
}

func TestReusedPIDIsNewTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for range 5 {
		h.poll(rd("T", 1, 5))
	}
	reused := rd("T", 1, 5)
	reused.Start = 2
	for range 3 {
		h.poll(reused)
	}
	if n := len(h.incidents()); n != 0 {
		t.Fatalf("incidents = %d, want 0: a reused pid must not inherit stuck time", n)
	}
}

func TestClearSemantics(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if h.w.Clear("T") {
		t.Fatal("new watcher must answer false")
	}
	h.poll()
	if h.w.Clear("T") {
		t.Fatal("one clear poll must answer false")
	}
	h.poll()
	if !h.w.Clear("T") {
		t.Fatal("two clear polls must answer true")
	}

	h = newHarness(t)
	h.poll(rd("T", 1, 5))
	h.poll(rd("T", 1, 5))
	h.poll()
	if h.w.Clear("T") {
		t.Fatal("one clear poll after a stuck poll must answer false")
	}
	h.failPoll()
	if h.w.Clear("T") {
		t.Fatal("a failed poll must not count")
	}
	h.poll()
	if !h.w.Clear("T") {
		t.Fatal("two clear polls around a failed poll must answer true")
	}
}

func TestFailedPollKeepsState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for range 3 {
		h.poll(rd("T", 1, 5))
	}
	h.failPoll()
	h.failPoll()
	if h.w.Clear("T") {
		t.Fatal("a failed poll must not flip Clear to true")
	}
	h.poll(rd("T", 1, 5))
	if n := len(h.incidents()); n != 0 {
		t.Fatalf("incidents at 4 minutes = %d, want 0", n)
	}
	h.poll(rd("T", 1, 5))
	if n := len(h.incidents()); n != 1 {
		t.Fatalf("incidents at 5 minutes = %d, want 1", n)
	}
	if w, i := h.count("WARN"), h.count("INFO"); w != 1 || i != 1 {
		t.Fatalf("warn=%d info=%d, want 1/1:\n%s", w, i, h.logs)
	}
}

func TestThresholdsFromTimers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tm := incident.DefaultTimers()
	tm.StuckReadTasks = 2
	if err := incident.SaveTimers(t.Context(), h.db, tm); err != nil {
		t.Fatal(err)
	}
	two := []mediaagentapi.StuckReader{rd("T", 1, 5), rd("T", 2, 5)}
	h.poll(two...)
	h.poll(two...)
	if n := len(h.incidents()); n != 1 {
		t.Fatalf("incidents = %d, want 1", n)
	}
}

func TestStartStopsOnCancel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.w.Start(ctx)
	cancel()
	done := make(chan struct{})
	go func() { h.w.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

func TestClearedTitlesForgotten(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var rs []mediaagentapi.StuckReader
	for i := range 20 {
		rs = append(rs, rd(fmt.Sprintf("T%d", i), 100+i, 5))
	}
	h.poll(rs...)
	h.poll(rs...)
	h.poll()
	h.poll()
	if n := h.w.TrackedTitlesForTest(); n != 0 {
		t.Fatalf("tracked titles after two clear polls = %d, want 0", n)
	}
	if !h.w.Clear("T3") {
		t.Fatal("a forgotten title must answer clear")
	}
}

func (h *harness) otherIncident(status db.IncidentStatus) *db.Incident {
	h.t.Helper()
	inc := &db.Incident{Status: status, Source: "discord", ReportedBy: "u", What: "cant_play", Title: "T"}
	if err := h.db.CreateIncident(h.t.Context(), inc); err != nil {
		h.t.Fatal(err)
	}
	return inc
}

func (h *harness) stuckReadIncidents() []*db.Incident {
	h.t.Helper()
	var out []*db.Incident
	for _, inc := range h.incidents() {
		if inc.What == agent.WhatStuckRead {
			out = append(out, inc)
		}
	}
	return out
}

func TestInactiveOtherIncidentGetsNewStuckRead(t *testing.T) {
	t.Parallel()
	for _, status := range []db.IncidentStatus{db.StatusAgentFixed, db.StatusManualTestNeeded, db.StatusBlocked} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			other := h.otherIncident(status)
			for range 6 {
				h.poll(three("T", 7)...)
			}
			if got := h.status(other.ID); got != status {
				t.Errorf("cant_play status = %s, want %s (never rerun a non-stuck_read incident)", got, status)
			}
			if n := len(h.stuckReadIncidents()); n != 1 {
				t.Fatalf("stuck_read incidents = %d, want 1", n)
			}
			for range 4 {
				h.poll(three("T", 7)...)
			}
			if n := len(h.stuckReadIncidents()); n != 1 {
				t.Errorf("stuck_read incidents after more polls = %d, want 1", n)
			}
		})
	}
}

func TestActiveOtherIncidentLeftAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	other := h.otherIncident(db.StatusInvestigating)
	for range 6 {
		h.poll(three("T", 7)...)
	}
	if got := h.status(other.ID); got != db.StatusInvestigating {
		t.Errorf("cant_play status = %s, want investigating", got)
	}
	if n := len(h.incidents()); n != 1 {
		t.Errorf("incidents = %d, want 1 (no stuck_read while another run is active)", n)
	}
	reps, err := h.db.ListReporters(t.Context(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(reps, "media-agent") {
		t.Errorf("the stuck read was reported onto the active %s incident", other.What)
	}
}
