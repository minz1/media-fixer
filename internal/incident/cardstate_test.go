package incident_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
)

const tooManyPlanFiles = 16

type cardNotifier struct {
	mu       sync.Mutex
	owner    []card.Card
	users    []card.Card
	ownerErr error
	userErr  error
}

func (n *cardNotifier) NotifyOwner(_ context.Context, c card.Card) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.owner = append(n.owner, c)
	return n.ownerErr
}

func (n *cardNotifier) NotifyUser(_ context.Context, _ string, c card.Card) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.users = append(n.users, c)
	return n.userErr
}

func (n *cardNotifier) ownerCards() []card.Card {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.owner)
}

func newCardService(t *testing.T, notif incident.Notifier, logs *bytes.Buffer) (*incident.Service, *db.DB, string) {
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
	logger := slog.New(slog.DiscardHandler)
	if logs != nil {
		logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	svc := incident.NewService(context.Background(), database, nil, nil, nil, nil, notif, logger)
	return svc, database, f.Name()
}

func cardIncident(t *testing.T, d *db.DB, status db.IncidentStatus, action string) *db.Incident {
	t.Helper()
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "discord", What: "cant_play", Title: "The Boys"}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if action != "" {
		finding := agent.DiagnosticResult{RootCause: "bad release", EscalateAction: action}
		if err := d.SetIncidentFinding(ctx, inc.ID, finding, finding); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateIncidentStatus(ctx, inc.ID, status); err != nil {
		t.Fatal(err)
	}
	inc.Status = status
	return inc
}

func storePlan(t *testing.T, d *db.DB, id string, plan any) string {
	t.Helper()
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetEscalationPlan(context.Background(), id, b); err != nil {
		t.Fatal(err)
	}
	return card.Fingerprint(b)
}

func agePlan(t *testing.T, dbPath, id string, age time.Duration) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath+"?_time_format=sqlite&_timezone=UTC")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(context.Background(),
		`UPDATE incidents SET escalation_plan_at = ? WHERE id = ?`, time.Now().Add(-age), id); err != nil {
		t.Fatal(err)
	}
}

func renderInc(t *testing.T, svc *incident.Service, id string) card.Update {
	t.Helper()
	up, ok, err := svc.CardRenderer()(context.Background(), card.IncidentSubject(id))
	if err != nil || !ok {
		t.Fatalf("render: ok=%v err=%v", ok, err)
	}
	return up
}

func buttonVerbsOf(t *testing.T, bs []card.Button) []string {
	t.Helper()
	var out []string
	for _, b := range bs {
		a, err := card.Decode(b.ID)
		if err != nil {
			t.Fatalf("button %q: %v", b.ID, err)
		}
		out = append(out, a.Verb)
	}
	return out
}

func readablePlan(files int) *client.ReplacePlan {
	p := &client.ReplacePlan{
		Title: "The Boys", Scope: client.ReplaceScopeEpisode, SeasonNumber: 1, EpisodeNumber: 2,
		Reason:      "corrupt video stream",
		Readability: &client.ReadabilityProbe{Checked: true, Probed: 1, ReadableFile: "/tv/The Boys/S01E02.mkv"},
	}
	for i := range files {
		p.Files = append(p.Files, client.ReplaceFile{ID: i + 1, Path: fmt.Sprintf("/tv/The Boys/S01E%02d.mkv", i+1)})
	}
	return p
}

func TestRenderer_ButtonRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status db.IncidentStatus
		action string
		setup  func(t *testing.T, d *db.DB, path, id string)
		want   []string
	}{
		{"resolved hides Resolve", db.StatusResolved, "", nil, []string{"rerun"}},
		{"open offers Resolve and Rerun", db.StatusOpen, "", nil, []string{"rerun", "resolve"}},
		{"locked offers Unlock", db.StatusBlocked, "", func(t *testing.T, d *db.DB, _, id string) {
			t.Helper()
			if err := d.SetAutonomousLocked(context.Background(), id, true); err != nil {
				t.Fatal(err)
			}
		}, []string{"rerun", "resolve", "unlock"}},
		{"keep searching with a pending outcome", db.StatusManualTestNeeded, agent.EscalateManualInvestigation,
			func(t *testing.T, d *db.DB, _, id string) {
				t.Helper()
				po := &db.PendingOutcome{MediaType: "tv", Title: "The Boys", Season: 1, Episode: 2}
				if err := d.SetPendingOutcome(context.Background(), id, po, time.Now()); err != nil {
					t.Fatal(err)
				}
			}, []string{"rerun", "resolve", "keep"}},
		{"keep searching needs manual_test_needed", db.StatusVerifying, "",
			func(t *testing.T, d *db.DB, _, id string) {
				t.Helper()
				po := &db.PendingOutcome{MediaType: "tv", Title: "The Boys", Season: 1, Episode: 2}
				if err := d.SetPendingOutcome(context.Background(), id, po, time.Now()); err != nil {
					t.Fatal(err)
				}
			}, []string{"rerun", "resolve"}},
		{"preview without a plan", db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch, nil,
			[]string{"preview", "rerun", "resolve"}},
		{"preview for a torrent re-add", db.StatusManualTestNeeded, agent.EscalateDeleteTorrentReadd, nil,
			[]string{"preview", "rerun", "resolve"}},
		{"no preview for manual investigation", db.StatusManualTestNeeded, agent.EscalateManualInvestigation, nil,
			[]string{"rerun", "resolve"}},
		{"stale plan offers Preview", db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch,
			func(t *testing.T, d *db.DB, path, id string) {
				t.Helper()
				storePlan(t, d, id, readablePlan(1))
				agePlan(t, path, id, expiredPlanAge)
			}, []string{"preview", "rerun", "resolve"}},
		{"fresh plan offers Approve and Refresh", db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch,
			func(t *testing.T, d *db.DB, _, id string) {
				t.Helper()
				storePlan(t, d, id, readablePlan(1))
			}, []string{"approve", "refresh", "rerun", "resolve"}},
		{"plan too long has no Approve", db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch,
			func(t *testing.T, d *db.DB, _, id string) {
				t.Helper()
				storePlan(t, d, id, readablePlan(tooManyPlanFiles))
			}, []string{"refresh", "rerun", "resolve"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc, d, path := newCardService(t, &cardNotifier{}, nil)
			inc := cardIncident(t, d, tc.status, tc.action)
			if tc.setup != nil {
				tc.setup(t, d, path, inc.ID)
			}
			got := buttonVerbsOf(t, renderInc(t, svc, inc.ID).Buttons)
			if !slices.Equal(got, tc.want) {
				t.Errorf("verbs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenderer_StatusLines(t *testing.T) {
	t.Parallel()
	want := map[db.IncidentStatus]string{
		db.StatusOpen:             "🆕 Open",
		db.StatusInvestigating:    "🔁 Investigating…",
		db.StatusVerifying:        "⏳ Verifying the fix",
		db.StatusReopened:         "🔁 Re-opened",
		db.StatusBlocked:          "⛔ Blocked",
		db.StatusAgentFixed:       "✅ Fixed by the agent",
		db.StatusManualTestNeeded: "🧑‍🔧 Needs you",
		db.StatusResolved:         "✅ Resolved",
	}
	for status, line := range want {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			svc, d, _ := newCardService(t, &cardNotifier{}, nil)
			inc := cardIncident(t, d, status, "")
			if got := renderInc(t, svc, inc.ID).Status; got != line {
				t.Errorf("status = %q, want %q", got, line)
			}
		})
	}
}

func TestRenderer_UnknownIncidentIsNotOK(t *testing.T) {
	t.Parallel()
	svc, _, _ := newCardService(t, &cardNotifier{}, nil)
	_, ok, err := svc.CardRenderer()(context.Background(), card.IncidentSubject("00000000-0000-0000-0000-000000000000"))
	if ok || err != nil {
		t.Errorf("ok=%v err=%v, want ok=false and no error", ok, err)
	}
}

func TestRenderer_PlanSection(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch)
	storePlan(t, d, inc.ID, readablePlan(2))

	secs := renderInc(t, svc, inc.ID).Sections
	if len(secs) < 5 {
		t.Fatalf("sections = %+v, want readability, reason, 2 files, search and expiry", secs)
	}
	if !strings.Contains(secs[0].Value, "A file this plan would delete currently reads fine") ||
		!strings.Contains(secs[0].Value, "/tv/The Boys/S01E02.mkv") {
		t.Errorf("first section = %+v, want the readability line", secs[0])
	}
	if secs[1].Value != "corrupt video stream" {
		t.Errorf("second section = %+v, want the Reason", secs[1])
	}
	if !strings.Contains(secs[2].Value, "S01E01.mkv") || !strings.Contains(secs[3].Value, "S01E02.mkv") {
		t.Errorf("file sections = %+v, %+v", secs[2], secs[3])
	}
	last := secs[len(secs)-1].Value
	if !strings.HasPrefix(last, "<t:") || !strings.HasSuffix(last, ":t>") {
		t.Errorf("last section = %q, want an expiry timestamp", last)
	}
}

func TestRenderer_PlanTooLong(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch)
	storePlan(t, d, inc.ID, readablePlan(tooManyPlanFiles))

	up := renderInc(t, svc, inc.ID)
	if len(up.Sections) != 1 || up.Sections[0].Value != "Plan too long for Discord; review it on the dashboard" {
		t.Errorf("sections = %+v, want only the too-long line", up.Sections)
	}
	if slices.Contains(buttonVerbsOf(t, up.Buttons), "approve") {
		t.Error("Approve offered for a plan that does not fit")
	}
}

func TestRenderer_ReaddPlanSection(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateDeleteTorrentReadd)
	fp := storePlan(t, d, inc.ID, &client.ReaddPlan{Name: "The.Boys.S01", Provider: "torbox", State: "error"})

	up := renderInc(t, svc, inc.ID)
	var text []string
	for _, s := range up.Sections {
		text = append(text, s.Value)
	}
	joined := strings.Join(text, "\n")
	for _, want := range []string{"The.Boys.S01", "torbox", "error"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sections %q missing %q", joined, want)
		}
	}
	i := slices.IndexFunc(up.Buttons, func(b card.Button) bool { return strings.HasPrefix(b.ID, "mf1:inc:approve:") })
	if i < 0 {
		t.Fatalf("buttons = %+v, want Approve", up.Buttons)
	}
	if a, _ := card.Decode(up.Buttons[i].ID); a.Args[len(a.Args)-1] != fp {
		t.Errorf("approve args = %v, want the plan fingerprint %s last", a.Args, fp)
	}
}

func clickInc(
	t *testing.T,
	svc *incident.Service,
	verb, id string,
	status db.IncidentStatus,
	extra ...string,
) card.Result {
	t.Helper()
	res, err := svc.CardHandler()(context.Background(), card.Action{
		Kind: "inc", Verb: verb, Args: append([]string{id, string(status)}, extra...),
	})
	if err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	return res
}

func TestHandler_StatusSeenMismatchRefused(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusResolved, "")

	res := clickInc(t, svc, "rerun", inc.ID, db.StatusManualTestNeeded)
	if res.Line != "Refused: this incident is now resolved" {
		t.Errorf("line = %q", res.Line)
	}
	if got, _ := d.GetIncident(context.Background(), inc.ID); got.Status != db.StatusResolved {
		t.Errorf("status = %s, want resolved (no action)", got.Status)
	}
}

func TestHandler_ApproveReturnsConfirm(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch)
	fp := storePlan(t, d, inc.ID, readablePlan(1))

	res := clickInc(t, svc, "approve", inc.ID, db.StatusManualTestNeeded, fp)
	if got := buttonVerbsOf(t, res.Confirm); !slices.Equal(got, []string{"confirm", "cancel"}) {
		t.Fatalf("confirm verbs = %v", got)
	}
	if res.Confirm[0].Style != card.Danger {
		t.Errorf("confirm style = %v, want danger", res.Confirm[0].Style)
	}
	if a, _ := card.Decode(res.Confirm[0].ID); a.Args[len(a.Args)-1] != fp {
		t.Errorf("confirm args = %v, want fp %s last", a.Args, fp)
	}
	if _, _, err := d.GetEscalationPlan(context.Background(), inc.ID); err != nil {
		t.Errorf("approve consumed the plan: %v", err)
	}
}

func TestHandler_ConfirmLabelPerAction(t *testing.T) {
	t.Parallel()
	for action, label := range map[string]string{
		agent.EscalateRemoveAndSearch:    "Yes, delete and re-search",
		agent.EscalateDeleteTorrentReadd: "Yes, delete and re-add",
	} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			svc, d, _ := newCardService(t, &cardNotifier{}, nil)
			inc := cardIncident(t, d, db.StatusManualTestNeeded, action)
			fp := storePlan(t, d, inc.ID, readablePlan(1))
			res := clickInc(t, svc, "approve", inc.ID, db.StatusManualTestNeeded, fp)
			if len(res.Confirm) == 0 || res.Confirm[0].Label != label {
				t.Errorf("confirm = %+v, want label %q", res.Confirm, label)
			}
		})
	}
}

func TestHandler_ConfirmCallsApproveWithFP(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)
	fp := fx.preview(t, inc.ID)

	res := clickInc(t, fx.svc, "confirm", inc.ID, db.StatusManualTestNeeded, "000000000000")
	if !strings.HasPrefix(res.Line, "Refused: ") || ag.execCalls.Load() != 0 {
		t.Fatalf("wrong fp: line %q, exec calls %d; want a refusal and no run", res.Line, ag.execCalls.Load())
	}
	res = clickInc(t, fx.svc, "confirm", inc.ID, db.StatusManualTestNeeded, fp)
	if strings.HasPrefix(res.Line, "Refused") || ag.execCalls.Load() != 1 {
		t.Errorf("line %q, exec calls %d; want the plan run once", res.Line, ag.execCalls.Load())
	}
}

func TestHandler_CancelIsEmpty(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	inc := cardIncident(t, d, db.StatusManualTestNeeded, agent.EscalateRemoveAndSearch)
	if res := clickInc(t, svc, "cancel", inc.ID, db.StatusManualTestNeeded); res.Line != "" || res.Confirm != nil {
		t.Errorf("cancel = %+v, want an empty Result", res)
	}
}

func TestHandler_PreviewStoresPlan(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	fx := newEscalationFixture(t, ag)
	inc := fx.incident(t)

	res := clickInc(t, fx.svc, "preview", inc.ID, db.StatusManualTestNeeded)
	if strings.HasPrefix(res.Line, "Refused") {
		t.Fatalf("line = %q", res.Line)
	}
	if _, _, err := fx.db.GetEscalationPlan(context.Background(), inc.ID); err != nil {
		t.Errorf("no plan stored: %v", err)
	}
}

func TestHandler_RefusalLines(t *testing.T) {
	t.Parallel()
	ag := newEscalationAgent()
	fx := newEscalationFixture(t, ag)
	ctx := context.Background()

	inc := fx.incident(t)
	if res := clickInc(t, fx.svc, "approve", inc.ID, db.StatusManualTestNeeded, "000000000000"); !strings.HasPrefix(
		res.Line, "Refused: ") {
		t.Errorf("approve with no plan: line %q, want a refusal", res.Line)
	}
	fx.preview(t, inc.ID)
	if res := clickInc(t, fx.svc, "confirm", inc.ID, db.StatusManualTestNeeded, "000000000000"); res.Line !=
		"Refused: "+incident.ErrPlanChangedForTest().Error() {
		t.Errorf("confirm with a changed plan: line %q", res.Line)
	}
	agePlan(t, fx.dbPath, inc.ID, expiredPlanAge)
	if res := clickInc(t, fx.svc, "confirm", inc.ID, db.StatusManualTestNeeded, "000000000000"); !strings.Contains(
		res.Line, incident.ErrPlanExpiredForTest().Error()) || !strings.HasPrefix(res.Line, "Refused: ") {
		t.Errorf("confirm with an expired plan: line %q", res.Line)
	}

	agentFixed := fx.incident(t)
	if err := fx.db.UpdateIncidentStatus(ctx, agentFixed.ID, db.StatusAgentFixed); err != nil {
		t.Fatal(err)
	}
	if res := clickInc(t, fx.svc, "confirm", agentFixed.ID, db.StatusAgentFixed, "000000000000"); res.Line !=
		"Refused: "+incident.ErrNotAwaitingDecisionForTest().Error() {
		t.Errorf("confirm when not awaiting a decision: line %q", res.Line)
	}

	resolved := fx.incident(t)
	if err := fx.svc.Resolve(ctx, resolved.ID); err != nil {
		t.Fatal(err)
	}
	if err := fx.db.UpdateIncidentStatus(ctx, resolved.ID, db.StatusResolved); err != nil {
		t.Fatal(err)
	}
	if res := clickInc(t, fx.svc, "resolve", resolved.ID, db.StatusResolved); res.Line !=
		"Refused: "+incident.ErrAlreadyResolved.Error() {
		t.Errorf("resolve when already resolved: line %q", res.Line)
	}
}

func TestOwnerCards_HaveSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("escalation", func(t *testing.T) {
		t.Parallel()
		notif := &cardNotifier{}
		svc, d, _ := newCardService(t, notif, nil)
		inc := cardIncident(t, d, db.StatusInvestigating, "")
		svc.EscalateToOwnerForTest(ctx, inc, "needs you")
		assertSubject(t, notif.ownerCards(), card.IncidentSubject(inc.ID))
	})
	t.Run("systemic lock", func(t *testing.T) {
		t.Parallel()
		notif := &cardNotifier{}
		svc, _, _ := newCardService(t, notif, nil)
		var inc *db.Incident
		for i := range systemicThresholdTitles + 1 {
			var err error
			inc, err = svc.Handle(ctx, &incident.Report{
				Source: "seerr", ReportedBy: "x", What: "cant_play", Title: fmt.Sprintf("T%d", i),
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		assertSubject(t, notif.ownerCards(), card.IncidentSubject(inc.ID))
	})
	t.Run("stale rerun", func(t *testing.T) {
		t.Parallel()
		database, inc := newReopenedIncident(t, staleAge)
		notif := &cardNotifier{}
		ag := newAlwaysFixedAgent()
		svc := incident.NewService(ctx, database, nil, ag, nil, nil, notif, slog.New(slog.DiscardHandler))
		svc.SweepStaleRuns(ctx)
		waitForRun(t, ag)
		assertSubject(t, notif.ownerCards(), card.IncidentSubject(inc.ID))
	})
	t.Run("could not DM reporter", func(t *testing.T) {
		t.Parallel()
		notif := &cardNotifier{userErr: errors.New("no mutual guilds")}
		svc, _, _ := newCardService(t, notif, nil)
		inc, err := svc.Handle(ctx, &incident.Report{
			Source: "discord", ReportedBy: "bob", ReporterDiscordID: "42", What: "cant_play", Title: "Lost",
		})
		if err != nil {
			t.Fatal(err)
		}
		svc.EscalateToOwnerForTest(ctx, inc, "needs you")
		assertUntracked(t, notif.ownerCards(), "Could not DM reporter")
	})
	t.Run("seerr failed", func(t *testing.T) {
		t.Parallel()
		notif := &cardNotifier{}
		svc, _, _ := newCardService(t, notif, nil)
		svc.SetSeerr(&fakeSeerr{err: errors.New("seerr down")})
		if _, err := svc.Handle(ctx, seerrReport("7")); err != nil {
			t.Fatal(err)
		}
		assertUntracked(t, notif.ownerCards(), "Could not comment on Seerr issue")
	})
}

func assertSubject(t *testing.T, cards []card.Card, subject string) {
	t.Helper()
	if len(cards) == 0 {
		t.Fatal("no owner card sent")
	}
	for _, c := range cards {
		if c.Subject == subject {
			return
		}
	}
	t.Errorf("owner cards %+v, want one with subject %q", cards, subject)
}

func assertUntracked(t *testing.T, cards []card.Card, text string) {
	t.Helper()
	found := false
	for _, c := range cards {
		if !strings.Contains(c.Plain(), text) {
			continue
		}
		found = true
		if c.Subject != "" || len(c.Buttons) > 0 || len(c.Verbs) > 0 {
			t.Errorf("card %q: subject %q, buttons %v, verbs %v; want untracked with no actions",
				text, c.Subject, c.Buttons, c.Verbs)
		}
	}
	if !found {
		t.Errorf("no owner card containing %q in %+v", text, cards)
	}
}

func TestReporterCard_HasNoSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	notif := &cardNotifier{}
	svc, _, _ := newCardService(t, notif, nil)
	inc, err := svc.Handle(ctx, &incident.Report{
		Source: "discord", ReportedBy: "bob", ReporterDiscordID: "42", What: "cant_play", Title: "Lost",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.EscalateToOwnerForTest(ctx, inc, "needs you")

	assertSubject(t, notif.ownerCards(), card.IncidentSubject(inc.ID))
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.users) == 0 {
		t.Fatal("no reporter card sent")
	}
	for _, c := range notif.users {
		if c.Subject != "" || len(c.Buttons) > 0 || len(c.Verbs) > 0 {
			t.Errorf("reporter card %+v carries a subject or buttons", c)
		}
	}
}

func TestSystemicLock_UntrackedIsDelivered(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	notif := &cardNotifier{ownerErr: fmt.Errorf("%w", card.ErrUntracked)}
	svc, _, _ := newCardService(t, notif, &logs)
	for i := range systemicThresholdTitles + 1 {
		if _, err := svc.Handle(context.Background(), &incident.Report{
			Source: "seerr", ReportedBy: "x", What: "cant_play", Title: fmt.Sprintf("T%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(notif.ownerCards()) == 0 {
		t.Fatal("no systemic lock card sent")
	}
	if strings.Contains(logs.String(), "notify owner") {
		t.Errorf("logs = %q, want no notify owner error for an untracked card", logs.String())
	}
}

func TestRenderer_PlanSectionStuckReadNotProbed(t *testing.T) {
	t.Parallel()
	svc, d, _ := newCardService(t, &cardNotifier{}, nil)
	ctx := context.Background()
	inc := &db.Incident{Status: db.StatusOpen, Source: "media-agent", What: agent.WhatStuckRead, Title: "The Boys"}
	if err := d.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	finding := agent.DiagnosticResult{RootCause: "stuck read", EscalateAction: agent.EscalateRemoveAndSearch}
	if err := d.SetIncidentFinding(ctx, inc.ID, finding, finding); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded); err != nil {
		t.Fatal(err)
	}
	p := readablePlan(1)
	p.Readability = &client.ReadabilityProbe{}
	storePlan(t, d, inc.ID, p)

	if got := renderInc(t, svc, inc.ID).Sections[0].Value; got != "Not probed: reading a stuck file would hang" {
		t.Errorf("readability = %q, want the not-probed line", got)
	}
}
