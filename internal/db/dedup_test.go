package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
)

// seedIncidentWithStatus creates an incident at a given status, since
// CreateIncident always starts at whatever the caller set.
func seedIncidentWithStatus(t *testing.T, d *db.DB, title string, status db.IncidentStatus) *db.Incident {
	t.Helper()
	inc := &db.Incident{
		Status: status, Source: "discord", ReportedBy: "tester",
		What: "cant_play", Title: title,
	}
	if err := d.CreateIncident(context.Background(), inc); err != nil {
		t.Fatal(err)
	}
	// CreateIncident may default the status; force it so the test controls it.
	if inc.Status != status {
		if err := d.UpdateIncidentStatus(context.Background(), inc.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	return inc
}

// TestFindOpenByTitle_OnlyCollapsesIntoActivelyWorkedIncidents is the
// regression test for the report-swallowing outage. An incident in
// manual_test_needed or blocked is waiting on a human and is making no
// progress, so a new report of the same title must start fresh work rather
// than being absorbed into it. Production ran for ~3 weeks with a single
// escalated incident silently eating every later report of that title, with
// no time bound and no new investigation, because the old query excluded only
// resolved/reopened.
func TestFindOpenByTitle_OnlyCollapsesIntoActivelyWorkedIncidents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	collapses := []db.IncidentStatus{
		db.StatusOpen, db.StatusInvestigating, db.StatusVerifying, db.StatusReopened,
	}
	for _, st := range collapses {
		t.Run("collapses_into_"+string(st), func(t *testing.T) {
			t.Parallel()
			d := openTestDB(t)
			want := seedIncidentWithStatus(t, d, "Lanterns", st)

			got, err := d.FindOpenByTitle(ctx, "Lanterns")
			if err != nil {
				t.Fatalf("FindOpenByTitle(%s) = %v, want the existing incident", st, err)
			}
			if got.ID != want.ID {
				t.Errorf("collapsed into %s, want %s", got.ID, want.ID)
			}
		})
	}

	staleStatuses := []db.IncidentStatus{
		db.StatusManualTestNeeded, db.StatusBlocked, db.StatusAgentFixed, db.StatusResolved,
	}
	for _, st := range staleStatuses {
		t.Run("does_not_collapse_into_"+string(st), func(t *testing.T) {
			t.Parallel()
			d := openTestDB(t)
			seedIncidentWithStatus(t, d, "Lanterns", st)

			_, err := d.FindOpenByTitle(ctx, "Lanterns")
			if !errors.Is(err, db.ErrNotFound) {
				t.Errorf("FindOpenByTitle collapsed a new report into a %s incident "+
					"(err = %v); that incident is waiting on a human and will never "+
					"pick the report up", st, err)
			}
		})
	}
}

// TestFindOpenByTitle_TitleWildcardsAreLiteral pins the LIKE-escaping fix: the
// title is caller-supplied, so an unescaped % or _ acted as a wildcard and
// collapsed a report into an unrelated open incident.
func TestFindOpenByTitle_TitleWildcardsAreLiteral(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openTestDB(t)

	seedIncidentWithStatus(t, d, "Anything At All", db.StatusOpen)

	// "%" as a title would previously LIKE-match "Anything At All (…)" shapes
	// and, more importantly, any title at all via the wildcard.
	for _, probe := range []string{"%", "_nything At All", "Any%"} {
		if _, err := d.FindOpenByTitle(ctx, probe); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("FindOpenByTitle(%q) matched an unrelated incident (err = %v); "+
				"LIKE wildcards in a caller-supplied title must be literal", probe, err)
		}
	}
}

// TestCountOpenIncidents_DoesNotLatchOnEscalatedIncidents pins the
// systemic-failure guard fix. The guard locks autonomous action once several
// incidents are broken at once; counting every non-resolved row meant
// escalated incidents (which never self-resolve) accumulated forever, so after
// five had built up, every new incident was force-blocked permanently.
func TestCountOpenIncidents_DoesNotLatchOnEscalatedIncidents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openTestDB(t)

	for i, st := range []db.IncidentStatus{
		db.StatusManualTestNeeded, db.StatusManualTestNeeded, db.StatusManualTestNeeded,
		db.StatusBlocked, db.StatusAgentFixed, db.StatusResolved,
	} {
		seedIncidentWithStatus(t, d, fmt.Sprintf("Old %s %d", st, i), st)
	}

	n, err := d.CountOpenIncidents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("CountOpenIncidents = %d with only escalated/closed incidents, want 0: "+
			"a backlog awaiting a human must not permanently trip the systemic guard", n)
	}

	seedIncidentWithStatus(t, d, "Live One", db.StatusInvestigating)
	if n, err = d.CountOpenIncidents(ctx); err != nil || n != 1 {
		t.Errorf("CountOpenIncidents = %d (err %v) with one investigating incident, want 1", n, err)
	}
}
