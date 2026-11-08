package incident_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

func (e *lcEnv) render(t *testing.T, id string) card.Update {
	t.Helper()
	r, err := e.svc.CardRenderer()(context.Background(), card.IncidentSubject(id))
	if err != nil || r.Outcome != card.Render {
		t.Fatalf("render: outcome=%v err=%v", r.Outcome, err)
	}
	return r.Update
}

func (e *lcEnv) press(t *testing.T, verb, id, token string) card.Result {
	t.Helper()
	res, err := e.svc.CardHandler()(context.Background(), card.Action{
		Kind: "inc", Verb: verb, Args: []string{id, token},
	})
	if err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	return res
}

func (e *lcEnv) tokenOf(t *testing.T, id string) string {
	t.Helper()
	return "v" + strconv.FormatInt(e.snap(t, id).Version, 10)
}

func TestI6_3_KeepButtonOnlyForNoRelease(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	nr := lifecycletest.SeedReport(t, env.db, lcReport("No release"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	dp := lifecycletest.SeedReport(t, env.db, lcReport("Stalled"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonDownloadProblem,
	})
	if got := buttonVerbsOf(t, env.render(t, nr.ID).Buttons); !slices.Contains(got, "keep") {
		t.Errorf("no_release buttons %v, want keep", got)
	}
	if got := buttonVerbsOf(t, env.render(t, dp.ID).Buttons); slices.Contains(got, "keep") {
		t.Errorf("download_problem buttons %v offer keep", got)
	}
}

func TestI6_4_UnlockButtonOnBlocked(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Blocked"), lifecycletest.Spec{State: lifecycle.Blocked})
	if got := buttonVerbsOf(
		t,
		env.render(t, s.ID).Buttons,
	); !slices.Equal(
		got,
		[]string{"rerun", "resolve", "unlock"},
	) {
		t.Errorf("blocked buttons %v", got)
	}
}

func TestCard_OldTokenRefusedForCheckedVerbs(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Old keep"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	res := env.press(t, "keep", s.ID, string(db.StatusManualTestNeeded))
	if res.Line != "Refused: this changed, look again" {
		t.Fatalf("line %q", res.Line)
	}
	if got := env.snap(t, s.ID); got.Status != lifecycle.ManualTestNeeded {
		t.Fatalf("status %s", got.Status)
	}
}

func TestCard_OldTokenResolveGoesThroughLegality(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.seed(t, "Old resolve")
	if res := env.press(t, "resolve", s.ID, string(db.StatusOpen)); !strings.HasPrefix(res.Line, "✅ Resolved") {
		t.Fatalf("first resolve %q", res.Line)
	}
	if res := env.press(t, "resolve", s.ID, string(db.StatusOpen)); res.Line != "Refused: incident already resolved" {
		t.Fatalf("second resolve %q", res.Line)
	}
}

func TestCard_StaleVersionRefusedAndRefreshed(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Stale keep"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonNoRelease,
	})
	stale := "v" + strconv.FormatInt(env.snap(t, s.ID).Version-1, 10)
	if res := env.press(t, "keep", s.ID, stale); res.Line != "Refused: this changed, look again" {
		t.Fatalf("stale keep %q", res.Line)
	}
	for _, b := range env.render(t, s.ID).Buttons {
		a, err := card.Decode(b.ID)
		if err != nil || a.Args[1] != env.tokenOf(t, s.ID) {
			t.Fatalf("refreshed button %v %v", a, err)
		}
	}
}

func TestCard_StartupRerenderUsesVersionTokens(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.claimed(t, "Tokens")
	up := env.render(t, s.ID)
	if len(up.Buttons) == 0 {
		t.Fatal("no buttons")
	}
	for _, b := range up.Buttons {
		a, err := card.Decode(b.ID)
		if err != nil || len(a.Args) < 2 || a.Args[1] != env.tokenOf(t, s.ID) {
			t.Fatalf("button %+v (%v) does not carry v%d", a, err, s.Version)
		}
	}
}

func TestCard_BudgetSpentNote(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := env.claimed(t, "Spent")
	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.Locked, n.ActionCount = true, agent.MaxAutonomousActions })
	if up := env.render(t, s.ID); !strings.Contains(up.Status, "budget spent") {
		t.Fatalf("status %q", up.Status)
	}
}

func TestCard_PlanInterruptedOffersRerunResolveUnlockOnly(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(
		t,
		env.db,
		lcReport("Interrupted"),
		lifecycletest.Spec{State: lifecycle.ExecutingPlan},
	)
	env.svc.ReconcileForTest(t.Context())
	env.force(t, s.ID, func(n *lifecycle.Snapshot) { n.Locked = true })
	got := buttonVerbsOf(t, env.render(t, s.ID).Buttons)
	if !slices.Equal(got, []string{"rerun", "resolve", "unlock"}) {
		t.Fatalf("plan_interrupted buttons %v", got)
	}
}

func TestCard_PlanPartialShowsOneLinePartialResult(t *testing.T) {
	t.Parallel()
	env := newLifecycleEnv(t, nil)
	s := lifecycletest.SeedReport(t, env.db, lcReport("Partial"), lifecycletest.Spec{
		State: lifecycle.ManualTestNeeded, Reason: lifecycle.ReasonPlanPartial, Finding: lifecycletest.Escalatable(),
	})
	if err := env.db.LogAction(t.Context(), &db.ActionLog{
		IncidentID: s.ID, Action: agent.EscalateRemoveAndSearch, TriggeredBy: "owner", Status: db.ActionFailed,
		Result: `{"deleted_files":[{"id":1},{"id":2}],"blocklisted_grabs":[7]}`,
	}); err != nil {
		t.Fatal(err)
	}
	up := env.render(t, s.ID)
	var lines []string
	for _, f := range up.Sections {
		if f.Name == "Result" {
			lines = append(lines, f.Value)
		}
	}
	if len(lines) != 1 || lines[0] != "Partial: deleted 2 files, blocklisted 1, search not sent" {
		t.Fatalf("partial lines %q", lines)
	}
	if got := buttonVerbsOf(t, up.Buttons); slices.Contains(got, "preview") || slices.Contains(got, "approve") {
		t.Fatalf("plan_partial buttons %v", got)
	}
}
