package incident_test

import (
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

func TestStatusReaders_UseMeta(t *testing.T) {
	t.Parallel()
	for _, st := range lifecycle.States() {
		if got, want := incident.StatusLineForTest(db.IncidentStatus(st)), lifecycle.Meta(st).Line; got != want {
			t.Errorf("%s line %q, want %q", st, got, want)
		}
	}
	for _, st := range lifecycle.States() {
		inDB := slices.Contains(db.ActiveIncidentStatuses(), db.IncidentStatus(st))
		if inDB != lifecycle.Meta(st).Active {
			t.Errorf("%s active in db %v, in Meta %v", st, inDB, lifecycle.Meta(st).Active)
		}
	}
}
