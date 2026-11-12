package lifecycle_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/machinetest"
)

func TestIncidentMachine_Idempotence(t *testing.T) {
	t.Parallel()
	machinetest.Idempotence(t, incidentMachine(), []lifecycle.EventKind{
		lifecycle.OwnerResolve, lifecycle.OwnerUnlock, lifecycle.OwnerApprove, lifecycle.OwnerPreview,
		lifecycle.OwnerKeepSearching, lifecycle.OwnerResumeOp, lifecycle.DownloadObserved, lifecycle.RunFinished,
		lifecycle.StaleRun, lifecycle.Tick,
	})
}

func TestIncidentMachine_IdempotenceOwnerRerun(t *testing.T) {
	t.Parallel()
	t.Skip("R2.2")
	machinetest.Idempotence(t, incidentMachine(), []lifecycle.EventKind{lifecycle.OwnerRerun})
}

func TestIncidentMachine_IdempotenceVerifyObserved(t *testing.T) {
	t.Parallel()
	machinetest.Idempotence(t, incidentMachine(), []lifecycle.EventKind{lifecycle.VerifyObserved})
}

func TestIncidentMachine_Commutes(t *testing.T) {
	t.Parallel()
	machinetest.Commutes(t, incidentMachine(), [][2]lifecycle.EventKind{
		{lifecycle.OwnerResolve, lifecycle.Tick},
		{lifecycle.DownloadObserved, lifecycle.Tick},
	})
}
