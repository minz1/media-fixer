package db_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type rehearsalCounts struct {
	perStatus                          string
	locked, overBudget, po, plan, spec int
	children                           [5]int
	cleared                            int
	fk                                 int
	integrity                          string
}

func countInt(t *testing.T, raw *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := raw.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		return -1
	}
	return n
}

func rehearsalSnapshot(t *testing.T, raw *sql.DB) rehearsalCounts {
	t.Helper()
	ctx := context.Background()
	c := rehearsalCounts{children: childCounts(t, raw)}
	c.perStatus = strings.Join(
		strs(t, raw, `SELECT status || '=' || COUNT(*) FROM incidents GROUP BY status ORDER BY status`), " ")
	c.locked = countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE autonomous_locked = 1`)
	c.overBudget = countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE action_count > 3`)
	c.po = countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE pending_outcome IS NOT NULL`)
	c.plan = countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE escalation_plan IS NOT NULL`)
	c.spec = countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE verify_spec IS NOT NULL`)
	c.cleared = countInt(t, raw, `SELECT COUNT(*) FROM incident_events WHERE kind = 'migration_cleared'`)
	c.fk = countInt(t, raw, `SELECT COUNT(*) FROM pragma_foreign_key_check`)
	if err := raw.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&c.integrity); err != nil {
		t.Fatal(err)
	}
	return c
}

func logCounts(t *testing.T, label string, c rehearsalCounts) {
	t.Helper()
	t.Logf("%-10s incidents %s | locked %d | action_count>3 %d | pending_outcome %d | plan %d | verify_spec %d",
		label, c.perStatus, c.locked, c.overBudget, c.po, c.plan, c.spec)
	t.Logf("%-10s events %d actions %d reporters %d seerr %d cards %d | migration_cleared %d | fk %d | integrity %s",
		label, c.children[0], c.children[1], c.children[2], c.children[3], c.children[4], c.cleared, c.fk, c.integrity)
}

func masterDiff(a, b []string) []string {
	name := func(s string) string { return strings.SplitN(s, "|", 3)[1] }
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, name(x))
		}
	}
	for _, y := range b {
		if !slices.Contains(a, y) && !slices.Contains(out, name(y)) {
			out = append(out, name(y))
		}
	}
	return out
}

func dryRun(t *testing.T, raw *sql.DB) string {
	t.Helper()
	var runs, verify, downloads, waits int
	var paused sql.NullString
	err := raw.QueryRowContext(context.Background(), `
SELECT COALESCE(SUM(status IN ('open','reopened','investigating')), 0),
       COALESCE(SUM(status = 'verifying' AND pending_outcome IS NULL), 0),
       COALESCE(SUM(status = 'verifying' AND pending_outcome IS NOT NULL), 0),
       COALESCE(SUM(status IN ('blocked','agent_fixed','manual_test_needed')), 0),
       (SELECT value FROM settings WHERE key = 'autonomous_paused')
FROM incidents`).Scan(&runs, &verify, &downloads, &waits, &paused)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join([]string{
		"new_agent_runs=" + strconv.Itoa(
			runs,
		),
		"verify_checks=" + strconv.Itoa(verify),
		"download_checks=" + strconv.Itoa(downloads),
		"owner_waits=" + strconv.Itoa(waits),
		"paused=" + paused.String,
	}, " ")
}

func reconcilePlan(t *testing.T, d *db.DB, raw *sql.DB) lifecycle.Counts {
	t.Helper()
	ctx := context.Background()
	ids := strs(t, raw, `SELECT id FROM incidents`)
	var obs []lifecycle.Observed
	for _, id := range ids {
		s, serr := d.GetIncidentSnapshot(ctx, id)
		if serr != nil {
			t.Fatalf("snapshot: %v", serr)
		}
		if cerr := lifecycle.Check(s); cerr != nil {
			t.Errorf("a migrated row fails Check: %v", cerr)
		}
		obs = append(obs, lifecycle.Observed{Snap: s})
	}
	paused, _ := d.IsAutonomousPaused(ctx)
	p := lifecycle.Policy{Now: time.Now(), Paused: paused, StaleAfter: 15 * time.Minute}
	return lifecycle.Tally(lifecycle.Reconcile(obs, lifecycle.Liveness{}, p), paused)
}

func clearedShortIDs(t *testing.T, raw *sql.DB) []string {
	t.Helper()
	return strs(t, raw, `SELECT substr(incident_id, 1, 8) || ':' ||
		json_extract(payload, '$.column') FROM incident_events WHERE kind = 'migration_cleared' ORDER BY 1`)
}

func TestMigration21_RehearsalOnSnapshot(t *testing.T) {
	t.Parallel()
	src := os.Getenv("MF_SNAPSHOT")
	if src == "" {
		t.Skip("MF_SNAPSHOT not set")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rehearsal.db")
	copyFile(t, src, path)
	fresh := filepath.Join(dir, "fresh.db")
	if err := db.MigrateUpToForTest(fresh, 20); err != nil {
		t.Fatal(err)
	}
	freshRaw := rawOpen(t, fresh)
	raw := rawOpen(t, path)
	pre := master(t, raw)
	if diff := masterDiff(pre, master(t, freshRaw)); len(diff) == 0 {
		t.Log("pre-up sqlite_master vs fresh v20: equal")
	} else {
		t.Logf("pre-up sqlite_master vs fresh v20: differing %v", diff)
	}
	t.Logf("dry run: %s", dryRun(t, raw))
	logCheckExposure(t, raw)
	logCounts(t, "before 20", rehearsalSnapshot(t, raw))
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	logCounts(t, "after 21", rehearsalSnapshot(t, raw))
	logCleared(t, clearedShortIDs(t, raw))
	c := reconcilePlan(t, d, raw)
	t.Logf("reconciler plan: runs=%d checks=%d plans_lost=%d parked=%d", c.Runs, c.Checks, c.PlansLost, c.Parked)
	d.Close()
	raw.Close()
	strip22(t, path)
	var out bytes.Buffer
	if err = db.MigrateDown(context.Background(), path, 21, &out); err != nil {
		t.Fatalf("down: %v", err)
	}
	changed := regexp.MustCompile(`changed rows: \d+`).FindString(out.String())
	t.Logf("down %s", changed)
	raw = rawOpen(t, path)
	logCounts(t, "after down", rehearsalSnapshot(t, raw))
	if slices.Equal(pre, master(t, raw)) {
		t.Log("sqlite_master after down = before up: equal")
	} else {
		t.Errorf("sqlite_master after down differs: %v", masterDiff(pre, master(t, raw)))
	}
}

func logCleared(t *testing.T, cleared []string) {
	t.Helper()
	t.Logf("migration_cleared rows: %d", len(cleared))
	want := strings.Fields(os.Getenv("MF_EXPECT_CLEARED"))
	if len(want) == 0 {
		return
	}
	var gotIDs []string
	for _, c := range cleared {
		gotIDs = append(gotIDs, strings.SplitN(c, ":", 2)[0])
	}
	slices.Sort(want)
	slices.Sort(gotIDs)
	if slices.Equal(want, gotIDs) {
		t.Logf("cleared set matches MF_EXPECT_CLEARED (%d ids)", len(want))
	} else {
		t.Errorf("cleared set differs from MF_EXPECT_CLEARED: %d expected, %d cleared", len(want), len(gotIDs))
	}
}

func logCheckExposure(t *testing.T, raw *sql.DB) {
	t.Helper()
	for _, c := range []struct{ name, where string }{
		{"incident_status", `status NOT IN ('open','reopened','investigating','verifying','manual_test_needed',
			'agent_fixed','blocked','resolved')`},
		{"incident_blocked_locked", `status = 'blocked' AND autonomous_locked = 0`},
		{"incident_pending_outcome_owner", `pending_outcome IS NOT NULL
			AND status NOT IN ('verifying', 'manual_test_needed')`},
		{"incident_plan_owner", `escalation_plan IS NOT NULL AND status <> 'manual_test_needed'`},
		{"incident_verify_schedule", `status = 'verifying'`},
		{"incident_reason_owner (mtn needing a reason)", `status = 'manual_test_needed'`},
	} {
		t.Logf("check exposure %s: %d rows", c.name, countInt(t, raw, `SELECT COUNT(*) FROM incidents WHERE `+c.where))
	}
}
