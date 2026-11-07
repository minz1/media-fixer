package lifecycle

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/minz1/mediafixer/internal/operation"
)

var ErrIllegal = errors.New("lifecycle: illegal snapshot")

func Check(s Snapshot) error {
	for _, c := range []func(Snapshot) error{checkSets, checkOwnership, checkOp, checkSchedule, checkCounters} {
		if err := c(s); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrIllegal, s.ID, err)
		}
	}
	return nil
}

func checkSets(s Snapshot) error {
	if !slices.Contains(States(), s.Status) {
		return fmt.Errorf("I1: unknown status %q", s.Status)
	}
	if s.Reason != ReasonNone && !slices.Contains(Reasons(), s.Reason) {
		return fmt.Errorf("I1: unknown reason %q", s.Reason)
	}
	if s.Status == Blocked && !s.Locked {
		return errors.New("I2: blocked but unlocked")
	}
	if s.Parked && s.Status != Open && s.Status != Reopened {
		return fmt.Errorf("I3: parked in %s", s.Status)
	}
	if (s.Reason != ReasonNone) != (s.Status == ManualTestNeeded) {
		return fmt.Errorf("I6: reason %q in %s", s.Reason, s.Status)
	}
	return nil
}

func checkOwnership(s Snapshot) error {
	noRelease := s.Status == ManualTestNeeded && s.Reason == ReasonNoRelease
	if (s.Download != nil) != (s.Status == AwaitingDownload || noRelease) {
		return fmt.Errorf("I4: pending outcome %v in %s/%s", s.Download != nil, s.Status, s.Reason)
	}
	if s.Download != nil && !slices.Contains(Stages(), s.Download.Stage) {
		return fmt.Errorf("I9: unknown download stage %q", s.Download.Stage)
	}
	if noRelease && s.Download != nil && (s.Download.Stage != StageSearching || s.Download.GrabNotified) {
		return errors.New("I4: no_release outcome past searching")
	}
	if s.Plan != nil && s.Status != ManualTestNeeded && s.Status != ExecutingPlan {
		return fmt.Errorf("I5: plan in %s", s.Status)
	}
	if s.Status == ExecutingPlan && s.Plan == nil {
		return errors.New("I5: executing_plan without a plan")
	}
	if (s.Reason == ReasonPlanInterrupted || s.Reason == ReasonPlanPartial) && s.Plan != nil {
		return fmt.Errorf("I5: plan kept in %s", s.Reason)
	}
	return nil
}

func checkSchedule(s Snapshot) error {
	scheduled := s.Status == Verifying || s.Status == AwaitingDownload
	if !s.NextCheck.IsZero() != scheduled {
		return fmt.Errorf("I7: next check %v in %s", !s.NextCheck.IsZero(), s.Status)
	}
	if (s.Verify != nil) != (s.Status == Verifying) {
		return fmt.Errorf("I8: verify %v in %s", s.Verify != nil, s.Status)
	}
	if s.Verify == nil {
		return nil
	}
	if s.Verify.ChecksLeft < 1 {
		return fmt.Errorf("I8: %d checks left", s.Verify.ChecksLeft)
	}
	ext, limit := s.Verify.Spec.Extensions, s.In.Limits.ExtensionsMax
	if ext < 0 || (limit > 0 && ext > limit) {
		return fmt.Errorf("I8: %d extensions", ext)
	}
	return nil
}

func checkCounters(s Snapshot) error {
	if s.Version < 0 || s.Generation < 0 || s.ActionCount < 0 {
		return fmt.Errorf("I10: negative counter v%d g%d n%d", s.Version, s.Generation, s.ActionCount)
	}
	return nil
}

func checkOp(s Snapshot) error {
	if s.Op == nil {
		return nil
	}
	if !inPlanStates(s) {
		return fmt.Errorf("I13: op in %s/%s", s.Status, s.Reason)
	}
	if s.Status == ExecutingPlan && (s.Plan == nil || !bytes.Equal(s.Op.Source, s.Plan.JSON)) {
		return errors.New("I14: op source differs from the plan")
	}
	return nil
}

func CheckDecision(d Decision) error {
	if d.OpWrite == nil {
		return nil
	}
	want := operation.Subject{Kind: operation.SubjectIncident, ID: d.Next.ID}
	if d.OpWrite.Subject != want {
		return fmt.Errorf("%w: %s: I11: op write for %v", ErrIllegal, d.Next.ID, d.OpWrite.Subject)
	}
	if err := operation.Check(*d.OpWrite); err != nil {
		return fmt.Errorf("%w: %s: I11: %w", ErrIllegal, d.Next.ID, err)
	}
	return nil
}
