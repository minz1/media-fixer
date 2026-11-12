package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func pageOf(t *testing.T, sp lifecycletest.Spec) (string, string) {
	t.Helper()
	srv, database := newDashboardTestServer(t)
	s := lifecycletest.Seed(t, database, sp)
	return do(t, srv, http.MethodGet, "/media/incidents/"+s.ID, nil).Body.String(), s.ID
}

func TestDashboard_ActionsFromMachine(t *testing.T) {
	t.Parallel()
	body, id := pageOf(t, lifecycletest.Spec{Title: "Blocked", State: lifecycle.Blocked})
	for _, path := range []string{"/rerun", "/resolve", "/unlock"} {
		if !strings.Contains(body, `hx-post="/media/incidents/`+id+path+`"`) {
			t.Errorf("blocked page has no %s button", path)
		}
	}
	if !strings.Contains(body, `hx-vals='{"v":"v`) {
		t.Error("buttons carry no version token")
	}
	body, id = pageOf(t, lifecycletest.Spec{Title: "Running", State: lifecycle.ExecutingPlan})
	if strings.Contains(body, `/incidents/`+id+`/rerun"`) {
		t.Error("a running plan offers Rerun")
	}
}

func TestDashboard_StaleVersionRedirectsWithNotice(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := seedNoRelease(t, database, "Stale")
	s, err := database.GetIncidentSnapshot(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := url.Values{"v": {"v" + strconv.FormatInt(s.Version-1, 10)}}
	rec := do(t, srv, http.MethodPost, "/media/incidents/"+inc.ID+"/keep-searching", stale)
	if rec.Code != http.StatusSeeOther || !strings.HasSuffix(rec.Header().Get("Location"), "?done=stale") {
		t.Fatalf("stale keep: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	page := do(t, srv, http.MethodGet, "/media/incidents/"+inc.ID+"?done=stale", nil).Body.String()
	if !strings.Contains(page, "tone-yellow\">Refused: this changed, look again.") {
		t.Error("the stale notice is missing or not in the warning tone")
	}
}

func TestDashboard_NoUnlockOnResolved(t *testing.T) {
	t.Parallel()
	body, id := pageOf(t, lifecycletest.Spec{Title: "Locked done", State: lifecycle.Resolved, Locked: true})
	if strings.Contains(body, `/incidents/`+id+`/unlock"`) {
		t.Error("a resolved incident offers Unlock")
	}
}

func TestDashboard_NewStatesHaveBadgeAndInfo(t *testing.T) {
	t.Parallel()
	for _, st := range []lifecycle.State{lifecycle.AwaitingDownload, lifecycle.ExecutingPlan} {
		body, _ := pageOf(t, lifecycletest.Spec{Title: string(st), State: st})
		m := lifecycle.Meta(st)
		if !strings.Contains(body, ">"+m.Label+"</span>") || !strings.Contains(body, `data-tip="`+m.Info+`"`) {
			t.Errorf("%s page lacks its badge or info", st)
		}
	}
}

func TestDashboard_ParkedBadge(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	if err := database.SetSetting(context.Background(), "autonomous_paused", "true"); err != nil {
		t.Fatal(err)
	}
	r := lifecycletest.Report("Parked", "")
	l := lifecycletest.Limits()
	a, err := database.CreateFromReport(
		context.Background(),
		r,
		lifecycletest.Decide(lifecycle.Event{Kind: lifecycle.Reported}, l, time.Now()),
	)
	if err != nil {
		t.Fatal(err)
	}
	body := do(t, srv, http.MethodGet, "/media/incidents/"+a.Snap.ID, nil).Body.String()
	if !strings.Contains(body, ">paused</span>") || !strings.Contains(body, "Runs when autonomous actions resume") {
		t.Error("a parked incident has no paused badge")
	}
}

func TestDashboard_RefusalRedirectsWithNotice(t *testing.T) {
	t.Parallel()
	srv, database := newDashboardTestServer(t)
	inc := seedNoRelease(t, database, "Not locked")
	s, err := database.GetIncidentSnapshot(context.Background(), inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := url.Values{"v": {"v" + strconv.FormatInt(s.Version, 10)}}
	rec := do(t, srv, http.MethodPost, "/media/incidents/"+inc.ID+"/unlock", v)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unlock of an unlocked incident: %d %s", rec.Code, rec.Body.String())
	}
	page := do(t, srv, http.MethodGet, rec.Header().Get("Location"), nil).Body.String()
	if !strings.Contains(page, "tone-yellow\">Refused: this incident is not locked.") {
		t.Error("the refusal notice is missing or not in the warning tone")
	}
}

func TestF20_DashboardHidesResolveDuringPlan(t *testing.T) {
	t.Parallel()
	body, id := pageOf(t, lifecycletest.Spec{Title: "Running", State: lifecycle.ExecutingPlan})
	if strings.Contains(body, `/incidents/`+id+`/resolve"`) {
		t.Error("a running plan offers Resolve")
	}
}
