package stuckqueue

import (
	"errors"
	"fmt"
	"slices"
)

var ErrIllegal = errors.New("stuckqueue: illegal snapshot")

func Check(s Snapshot) error {
	if s.State == StateUntracked {
		if s.Acted != nil || s.Import != nil || s.Misses != 0 || s.Offered != OfferNone {
			return fmt.Errorf("%w: %s: untracked carries data", ErrIllegal, s.Key.DownloadID)
		}
		return nil
	}
	for _, c := range []func(Snapshot) error{
		checkState, checkMisses, checkNotified, checkOffered, checkImport, checkActed, checkFiles, checkOp, checkOrder,
	} {
		if err := c(s); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrIllegal, s.Key.DownloadID, err)
		}
	}
	return nil
}

func checkState(s Snapshot) error {
	if !slices.Contains(States(), s.State) {
		return fmt.Errorf("C1: unknown state %q", s.State)
	}
	if !slices.Contains(Offers(), s.Offered) {
		return fmt.Errorf("C1: unknown offer %q", s.Offered)
	}
	return nil
}

func checkMisses(s Snapshot) error {
	if s.Misses != 0 && s.Misses != 1 {
		return fmt.Errorf("C2: misses %d", s.Misses)
	}
	if s.Misses != 0 && (s.State == StateImportInflight || s.State == StateActed) {
		return fmt.Errorf("C2: misses %d in %s", s.Misses, s.State)
	}
	return nil
}

func checkNotified(s Snapshot) error {
	if s.State == StateNotified && s.NotifiedAt.IsZero() {
		return errors.New("C3: notified without a notified time")
	}
	if s.State == StateDue && !s.NotifiedAt.IsZero() {
		return errors.New("C3: due with a notified time")
	}
	return nil
}

func checkOffered(s Snapshot) error {
	if s.State == StateWaiting && s.Offered != OfferRemove {
		return fmt.Errorf("C4: waiting offers %q", s.Offered)
	}
	if (s.State == StateImportInflight || s.State == StateActed) && s.Offered != OfferNone {
		return fmt.Errorf("C4: %s offers %q", s.State, s.Offered)
	}
	return nil
}

func checkImport(s Snapshot) error {
	if (s.State == StateImportInflight) != (s.Import != nil) {
		return fmt.Errorf("C5: state %s with import %v", s.State, s.Import != nil)
	}
	if s.Import != nil && s.Import.CommandID <= 0 {
		return fmt.Errorf("C5: command id %d", s.Import.CommandID)
	}
	return nil
}

func checkActed(s Snapshot) error {
	a := s.Acted
	if a != nil && (a.Action == "" || a.At.IsZero() || a.Result == "") {
		return fmt.Errorf("C6: partial acted %+v", *a)
	}
	switch s.State {
	case StateActed:
		if a == nil || (a.Result != ResultOK && a.Result != ResultUnfinished) {
			return fmt.Errorf("C7: acted with %+v", a)
		}
	case StateNotified:
		if a != nil && (a.Action != ActionImport || !isNote(a.Result)) {
			return fmt.Errorf("C7: notified with %+v", *a)
		}
	case StateUntracked, StateWaiting, StateDue, StateHeld, StateImportInflight, StateCleared:
		if a != nil {
			return fmt.Errorf("C7: %s with %+v", s.State, *a)
		}
	}
	return nil
}

func isNote(r ActResult) bool {
	return slices.Contains(
		[]ActResult{ResultImportFailed, ResultImportTimeout, ResultImportUnconfirmed, ResultPartial},
		r,
	)
}

func validFiles(f Files) bool { return f.N > 0 && f.N <= f.M }

func checkFiles(s Snapshot) error {
	if s.Import != nil && !validFiles(s.Import.Files) {
		return fmt.Errorf("C9: import files %+v", s.Import.Files)
	}
	if a := s.Acted; a != nil {
		if a.Action == ActionImport && !validFiles(a.Files) {
			return fmt.Errorf("C9: imported files %+v", a.Files)
		}
		if a.Action != ActionImport && a.Files != (Files{}) {
			return fmt.Errorf("C9: %s with files %+v", a.Action, a.Files)
		}
	}
	return nil
}

func checkOp(s Snapshot) error {
	a := s.Acted
	if a == nil {
		return nil
	}
	if a.Result == ResultUnfinished && a.Action != ActionRegrab {
		return fmt.Errorf("C8: unfinished %s", a.Action)
	}
	if a.Result == ResultUnfinished && a.OpID <= 0 {
		return errors.New("C10: unfinished without an op id")
	}
	if a.OpID > 0 && a.Action != ActionRegrab {
		return fmt.Errorf("C10: op id on %s", a.Action)
	}
	return nil
}

func checkOrder(s Snapshot) error {
	if !s.NotifiedAt.IsZero() && s.NotifiedAt.Before(s.FirstSeen) {
		return errors.New("notified before first seen")
	}
	if s.Acted != nil && s.Acted.At.Before(s.FirstSeen) {
		return errors.New("acted before first seen")
	}
	if s.Import != nil && s.Import.StartedAt.Before(s.FirstSeen) {
		return errors.New("import started before first seen")
	}
	return nil
}
