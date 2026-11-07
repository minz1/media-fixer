package incident_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
	"github.com/minz1/mediafixer/internal/journal"
	"github.com/minz1/mediafixer/internal/opexec"
)

type approvedFixture struct {
	svc  *incident.Service
	db   *db.DB
	disp *agent.Dispatcher
}

type fakeArr struct {
	failDelete  map[int]bool
	deleteState int
	failSearch  bool
	onDelete    func(id int)
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/episodefile/"):
		var id int
		_, _ = fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/api/v3/episodefile/"), "%d", &id)
		if f.onDelete != nil {
			f.onDelete(id)
		}
		if f.failDelete[id] {
			w.WriteHeader(cmp.Or(f.deleteState, http.StatusInternalServerError))
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
		if f.failSearch {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func failingReaddDecypharr(t *testing.T) *client.DecypharrClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/torrents/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.NewDecypharr(srv.URL, "token")
}

func newApprovedFixture(t *testing.T, arr *fakeArr, decypharr *client.DecypharrClient) *approvedFixture {
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
	base, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	jrnl := journal.New(database)
	log := slog.New(slog.DiscardHandler)

	disp := &agent.Dispatcher{Decypharr: decypharr}
	if arr != nil {
		srv := httptest.NewServer(arr)
		t.Cleanup(srv.Close)
		disp.Sonarr = client.NewArr(srv.URL, "key")
	}
	ag := agent.New(nil, "stub", disp, database, jrnl, log)
	svc := incident.NewService(base, database, jrnl, ag, nil, nil, &captureNotifier{}, log)
	svc.SetOpPorts(portsFor(disp))
	t.Cleanup(func() { stopService(t, svc) })
	return &approvedFixture{svc: svc, db: database, disp: disp}
}

func (fx *approvedFixture) awaiting(t *testing.T, action string, params map[string]any) *db.Incident {
	t.Helper()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "The Boys"}
	lifecycletest.Incident(t, fx.db, inc)
	finding := agent.DiagnosticResult{
		RootCause:        "bad release",
		PrimaryAction:    "manual_investigation",
		EscalateAction:   action,
		EscalateParams:   params,
		RequiresApproval: true,
	}
	lifecycletest.Finding(t, fx.db, inc.ID, finding)
	lifecycletest.MoveTo(t, fx.db, inc.ID, db.StatusManualTestNeeded)
	return inc
}

func (fx *approvedFixture) approve(t *testing.T, id string, plan any) error {
	t.Helper()
	ctx := context.Background()
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	lifecycletest.StorePlan(t, fx.db, id, planJSON)
	return fx.svc.ApproveEscalation(ctx, id, card.Fingerprint(planJSON), fx.svc.CurrentVersion(ctx, id))
}

func (fx *approvedFixture) actions(t *testing.T, id string) []*db.ActionLog {
	t.Helper()
	actions, err := fx.db.ListActions(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return actions
}

func describeActions(actions []*db.ActionLog) string {
	var sb strings.Builder
	for _, a := range actions {
		fmt.Fprintf(&sb, "\n  %s by %s: %s result=%q error=%q", a.Action, a.TriggeredBy, a.Status, a.Result, a.Error)
	}
	return sb.String()
}

func replaceParams() map[string]any {
	return map[string]any{
		"media_type": "tv", "title": "The Boys", "scope": "season", "season": 1, "reason": "wrong_content",
	}
}

func seasonPlan(files ...client.ReplaceFile) client.ReplacePlan {
	probe := &client.ReadabilityProbe{Checked: true, Probed: 1, ReadableFile: "/mnt/probe-ok.mkv", BytesRead: 1}
	return client.ReplacePlan{
		MediaType: "tv", Title: "The Boys", Scope: "season", MediaID: 2, SeasonNumber: 1,
		EpisodeNumber: -1, EpisodeID: -1, Files: files, SkipBlocklist: true,
		Reason: client.ReplaceReasonWrongContent, Readability: probe,
	}
}

func TestA2_3_ApprovedPlanLogsOnTheApprovedIncident(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	x := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())
	y := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())

	if err := fx.approve(t, y.ID, seasonPlan(client.ReplaceFile{ID: 10, Path: "/mnt/s01e01.mkv"})); err != nil {
		t.Fatal(err)
	}

	if got := fx.actions(t, x.ID); len(got) != 0 {
		t.Errorf("incident X, last run but never approved, got rows:%s", describeActions(got))
	}
	if got := fx.actions(t, y.ID); len(got) != 1 {
		t.Errorf("approved incident Y got %d rows, want 1:%s", len(got), describeActions(got))
	}
}

func TestA2_4_ApproveBeforeAnyRunRecordsTheExecution(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	y := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())

	if err := fx.approve(t, y.ID, seasonPlan(client.ReplaceFile{ID: 10, Path: "/mnt/s01e01.mkv"})); err != nil {
		t.Fatal(err)
	}

	got := fx.actions(t, y.ID)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1:%s", len(got), describeActions(got))
	}
	row := got[0]
	if row.Status != db.ActionApplied {
		t.Errorf("status = %q, want applied", row.Status)
	}
	for _, want := range []string{"/mnt/s01e01.mkv", "SeasonSearch", `"readable_file":"/mnt/probe-ok.mkv"`} {
		if !strings.Contains(row.Result, want) {
			t.Errorf("result %q is missing %s", row.Result, want)
		}
	}
}

func TestA2_5_OwnerExecutionLogsOneRow(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{}, nil)
	y := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())

	if err := fx.approve(t, y.ID, seasonPlan(client.ReplaceFile{ID: 10, Path: "/mnt/s01e01.mkv"})); err != nil {
		t.Fatal(err)
	}

	got := fx.actions(t, y.ID)
	if len(got) != 1 {
		t.Fatalf("one approval logged %d rows, want 1:%s", len(got), describeActions(got))
	}
	if got[0].Action != agent.EscalateRemoveAndSearch || got[0].TriggeredBy != "owner" {
		t.Errorf("row = %s by %s, want %s by owner", got[0].Action, got[0].TriggeredBy, agent.EscalateRemoveAndSearch)
	}
	if phrase := agent.UserPhrase(got[0].Action); phrase == "" {
		t.Errorf("UserPhrase(%q) is empty; progress notes would drop the approved replace", got[0].Action)
	}
}

func TestA9_3_FailedApprovedReaddIsLoggedFailed(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, nil, failingReaddDecypharr(t))
	y := fx.awaiting(t, agent.EscalateDeleteTorrentReadd, map[string]any{"name": "The.Boys.S01"})

	err := fx.approve(t, y.ID, client.ReaddPlan{
		Name: "The.Boys.S01", InfoHash: "abc", Category: "sonarr", Magnet: "magnet:?xt=urn:btih:abc",
	})
	if err == nil {
		t.Fatal("approve succeeded, want the re-add failure")
	}

	got := fx.actions(t, y.ID)
	if len(got) == 0 {
		t.Fatal("a re-add that deleted the torrent left no row")
	}
	for _, row := range got {
		if row.Status != db.ActionFailed {
			t.Errorf("row %s by %s is %q, want failed:%s",
				row.Action, row.TriggeredBy, row.Status, describeActions(got))
		}
	}
	if !strings.Contains(got[len(got)-1].Result, `"deleted":true`) {
		t.Errorf("result %q does not record that the torrent was deleted", got[len(got)-1].Result)
	}
}

func TestS9_8_ReplaceKeepsItsPartialResult(t *testing.T) {
	t.Parallel()
	fx := newApprovedFixture(t, &fakeArr{failDelete: map[int]bool{11: true}}, nil)
	y := fx.awaiting(t, agent.EscalateRemoveAndSearch, replaceParams())

	err := fx.approve(t, y.ID, seasonPlan(
		client.ReplaceFile{ID: 10, Path: "/mnt/s01e01.mkv"},
		client.ReplaceFile{ID: 11, Path: "/mnt/s01e02.mkv"},
	))
	if err == nil {
		t.Fatal("approve succeeded, want the delete failure")
	}

	got := fx.actions(t, y.ID)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1:%s", len(got), describeActions(got))
	}
	row := got[0]
	if row.Status != db.ActionFailed || row.Error == "" {
		t.Errorf("row = %s error %q, want failed with the error", row.Status, row.Error)
	}
	var res struct {
		DeletedFiles    []client.ReplaceFile `json:"deleted_files"`
		SearchTriggered string               `json:"search_triggered"`
	}
	if decodeErr := json.Unmarshal([]byte(row.Result), &res); decodeErr != nil {
		t.Fatalf("result %q: %v", row.Result, decodeErr)
	}
	if len(res.DeletedFiles) != 1 || res.DeletedFiles[0].ID != 10 || res.SearchTriggered != "" {
		t.Errorf("result = %+v, want file 10 deleted and no search", res)
	}
}

func portsFor(disp *agent.Dispatcher) opexec.Ports {
	p := opexec.Ports{Arr: func(app string) opexec.Arr {
		if app == "sonarr" && disp.Sonarr != nil {
			return disp.Sonarr
		}
		return nil
	}}
	if disp.Decypharr != nil {
		p.Decypharr = disp.Decypharr
	}
	return p
}
