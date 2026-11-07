package incident_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/incident/lifecycletest"
)

type recNotifier struct {
	mu         sync.Mutex
	owner      []card.Card
	users      []card.Card
	ownerFails int
}

func (n *recNotifier) NotifyOwner(_ context.Context, c card.Card) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ownerFails > 0 {
		n.ownerFails--
		return errors.New("discord down")
	}
	n.owner = append(n.owner, c)
	return nil
}

func (n *recNotifier) NotifyUser(_ context.Context, _ string, c card.Card) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.users = append(n.users, c)
	return nil
}

func (n *recNotifier) userCards() []card.Card {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]card.Card(nil), n.users...)
}

type lcEnv struct {
	path  string
	svc   *incident.Service
	db    *db.DB
	notif *recNotifier
	logs  *syncBuffer
}

func newLifecycleEnv(t *testing.T, ag incident.AgentRunner) *lcEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lc.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	env := &lcEnv{path: path, db: database, notif: &recNotifier{}, logs: &syncBuffer{}}
	logger := slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	env.svc = incident.NewService(t.Context(), database, nil, ag, nil, nil, env.notif, logger)
	t.Cleanup(func() { stopService(t, env.svc) })
	return env
}

func lcReport(title string) *db.NewIncident {
	return &db.NewIncident{Source: "discord", ReportedBy: "someone", ReporterDiscordID: "u-1", What: "cant_play",
		Title: title}
}

func lcDecide(ev lifecycle.Event) db.Decide {
	return func(cur lifecycle.Snapshot) (db.Decision, error) {
		cur.In.Limits = lifecycle.Limits{MaxActions: 3, SystemicThreshold: 5, VerifyChecks: 3, ExtensionsMax: 3,
			Minute: time.Minute, PlanTTL: time.Hour}
		d, err := lifecycle.Next(cur, ev, time.Now())
		return db.Decision{Decision: d}, err
	}
}

func (e *lcEnv) seed(t *testing.T, title string) lifecycle.Snapshot {
	t.Helper()
	a, err := e.db.CreateFromReport(
		context.Background(),
		lcReport(title),
		lcDecide(lifecycle.Event{Kind: lifecycle.Reported}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return a.Snap
}

func (e *lcEnv) claimed(t *testing.T, title string) lifecycle.Snapshot {
	t.Helper()
	s := e.seed(t, title)
	a, err := e.db.ApplyIncident(
		context.Background(),
		s.ID,
		lcDecide(lifecycle.Event{Kind: lifecycle.RunClaimed, Gen: s.Generation}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return a.Snap
}

func (e *lcEnv) snap(t *testing.T, id string) lifecycle.Snapshot {
	t.Helper()
	s, err := e.db.GetIncidentSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(notifyWaitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *lcEnv) logged(substr string) bool { return strings.Contains(e.logs.String(), substr) }

func (e *lcEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", e.path+"?_pragma=busy_timeout(5000)&_time_format=sqlite&_timezone=UTC")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err = raw.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

const statusAwaiting = lifecycle.AwaitingDownload

func seedTracked(t *testing.T, database *db.DB, inc *db.Incident) {
	t.Helper()
	sp := lifecycletest.Spec{Title: inc.Title, What: inc.What, State: inc.Status}
	if inc.Status == db.StatusVerifying || inc.Status == statusAwaiting {
		sp.State = lifecycle.AwaitingDownload
	}
	if inc.Status == db.StatusManualTestNeeded {
		sp.Reason = lifecycle.ReasonNoRelease
	}
	r := &db.NewIncident{Source: inc.Source, ReportedBy: inc.ReportedBy, What: inc.What, Title: inc.Title}
	s := lifecycletest.SeedReport(t, database, r, sp)
	inc.ID = s.ID
}

func setTrack(ctx context.Context, database *db.DB, id string, po *db.PendingOutcome, next time.Time) error {
	_, err := database.ApplyIncident(ctx, id, lifecycletest.TrackDecide(po, next))
	return err
}

func dueNow(t *testing.T, database *db.DB, id string) {
	t.Helper()
	_, err := database.ApplyIncident(t.Context(), id, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		n.NextCheck = time.Now().Add(-time.Second)
		return db.Decision{Next: n}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (n *recNotifier) ownerCards() []card.Card {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]card.Card(nil), n.owner...)
}

func (e *lcEnv) force(t *testing.T, id string, edit func(s *lifecycle.Snapshot)) {
	t.Helper()
	_, err := e.db.ApplyIncident(t.Context(), id, func(cur lifecycle.Snapshot) (db.Decision, error) {
		n := cur.Clone()
		edit(&n)
		return db.Decision{Next: n}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *lcEnv) version(t *testing.T, id string) incident.Version {
	t.Helper()
	return incident.Version{N: e.snap(t, id).Version, Set: true}
}
