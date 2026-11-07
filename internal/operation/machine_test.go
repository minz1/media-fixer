package operation_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/machinetest"
	"github.com/minz1/mediafixer/internal/operation"
)

func TestOperationMachine_Totality(t *testing.T) {
	t.Parallel()
	machinetest.Totality(t, opMachine())
}

func TestOperationMachine_Reachability(t *testing.T) {
	t.Parallel()
	machinetest.Reachability(t, opMachine())
}

func TestOperationMachine_Invariants(t *testing.T) {
	t.Parallel()
	for _, s := range operation.Statuses() {
		for _, op := range samples(s) {
			if err := operation.Check(op); err != nil {
				t.Errorf("sample %s is illegal: %v", key(op), err)
			}
		}
	}
	machinetest.Invariants(t, opMachine())
}

func TestOperationMachine_OnlyOwnerLeavesFailed(t *testing.T) {
	t.Parallel()
	for _, op := range samples(operation.FailedAtStep) {
		for _, k := range operation.EventKinds() {
			for _, e := range eventsFor(k) {
				next, err := apply(op, e)
				if err != nil {
					continue
				}
				if k != operation.OwnerContinued || next.Status == operation.FailedAtStep {
					t.Errorf("%s: %s accepted into %s", key(op), k, next.Status)
				}
			}
		}
		if operation.Resumable(op) {
			if _, err := apply(op, tev{ev: resumed(0), probes: "todo"}); err != nil {
				t.Errorf("%s: resumable but the owner is refused: %v", key(op), err)
			}
		}
	}
}

func checkRejections(t *testing.T, op operation.Op, k operation.EventKind) {
	t.Helper()
	for _, e := range eventsFor(k) {
		next, err := apply(op, e)
		if err == nil {
			continue
		}
		if key(next) != key(op) {
			t.Errorf("%s: %s rejected but changed the op", key(op), k)
		}
		if operation.CodeOf(err) == "" {
			t.Errorf("%s: %s rejected without a code: %v", key(op), k, err)
		}
	}
}

func TestOperationMachine_RejectionsLeaveTheOpUntouched(t *testing.T) {
	t.Parallel()
	for _, s := range operation.Statuses() {
		for _, op := range samples(s) {
			for _, k := range operation.EventKinds() {
				checkRejections(t, op, k)
			}
		}
	}
}
