package db_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func TestMigration21_StatusSetMatchesStates(t *testing.T) {
	t.Parallel()
	m := regexp.MustCompile(`(?s)CONSTRAINT incident_status CHECK \(status IN \((.*?)\)\)`).
		FindStringSubmatch(db.IncidentsLifecycleDDLForTest)
	if m == nil {
		t.Fatal("no incident_status CHECK in the migration DDL")
	}
	var got []string
	for s := range strings.SplitSeq(m[1], ",") {
		got = append(got, strings.Trim(strings.TrimSpace(s), "'"))
	}
	var want []string
	for _, s := range lifecycle.States() {
		want = append(want, string(s))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("CHECK status set %v, lifecycle states %v", got, want)
	}
}

func TestMigration21_ChecksRejectIllegalRows(t *testing.T) {
	t.Parallel()
	d := openTestDB(t)
	cases := map[string]string{
		"incident_status":                `status) VALUES (?, 'bogus'`,
		"incident_flags":                 `parked) VALUES (?, 2`,
		"incident_blocked_locked":        `status) VALUES (?, 'blocked'`,
		"incident_pending_outcome_owner": `pending_outcome) VALUES (?, '{}'`,
		"incident_download_has_outcome":  `status, verify_next_check) VALUES (?, 'awaiting_download', '2026-01-01'`,
		"incident_plan_owner":            `escalation_plan) VALUES (?, '{}'`,
		"incident_plan_claimed":          `status) VALUES (?, 'executing_plan'`,
		"incident_reason_owner":          `reason) VALUES (?, 'needs_owner'`,
		"incident_parked_owner":          `status, parked) VALUES (?, 'resolved', 1`,
		"incident_counters":              `version) VALUES (?, -1`,
		"incident_verify_schedule":       `status, verify_spec) VALUES (?, 'verifying', '{}'`,
		"incident_verify_spec":           `status, verify_spec) VALUES (?, 'resolved', '{}'`,
	}
	for name, tail := range cases {
		q := `INSERT INTO incidents (source, reported_by, what, title, id, ` +
			strings.Replace(tail, ") VALUES (?, ", ") VALUES (?, ?, ?, ?, ?, ", 1) + `)`
		err := d.ExecForTest(context.Background(), q,
			"discord", "x", "cant_play", "Check "+name, "check-"+name)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
