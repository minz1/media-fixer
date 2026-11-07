package incident

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/journal"
)

var ErrAlreadyResolved = lifecycle.ErrAlreadyResolved

type Version struct {
	N   int64
	Set bool
}

func ParseVersion(s string) Version {
	n, err := strconv.ParseInt(strings.TrimPrefix(s, "v"), 10, 64)
	if err != nil || !strings.HasPrefix(s, "v") || n < 0 {
		return Version{}
	}
	return Version{N: n, Set: true}
}

func (v Version) Token() string { return "v" + strconv.FormatInt(v.N, 10) }

func (s *Service) owner(ctx context.Context, verb, id string, kind lifecycle.EventKind, v Version) error {
	leave, err := s.ownerSection(verb, id)
	if err != nil {
		return err
	}
	defer leave()
	var extra eventsFor
	if kind == lifecycle.OwnerRerun {
		extra = s.ownerRerunEvents(ctx, id)
	}
	_, err = s.apply(ctx, id, lifecycle.Event{Kind: kind, Version: v.N, HasVersion: v.Set}, extra)
	return err
}

func (s *Service) Resolve(ctx context.Context, id string, v Version) error {
	return s.owner(ctx, verbResolve, id, lifecycle.OwnerResolve, v)
}

func (s *Service) Rerun(ctx context.Context, id string, v Version) error {
	return s.owner(ctx, verbRerun, id, lifecycle.OwnerRerun, v)
}

func (s *Service) Unlock(ctx context.Context, id string, v Version) error {
	return s.owner(ctx, verbUnlock, id, lifecycle.OwnerUnlock, v)
}

func (s *Service) KeepSearching(ctx context.Context, id string, v Version) error {
	return s.owner(ctx, verbKeep, id, lifecycle.OwnerKeepSearching, v)
}

func IsRefusal(err error) bool {
	for _, target := range []error{
		lifecycle.ErrChanged, lifecycle.ErrPlanRunning, lifecycle.ErrAlreadyResolved, lifecycle.ErrNotLocked,
		lifecycle.ErrResolved, lifecycle.ErrNoKeepSearching, lifecycle.ErrPlanHalfRun, lifecycle.ErrNotEscalatable,
		lifecycle.ErrNotAwaitingDecision, lifecycle.ErrNoPlan, lifecycle.ErrPlanExpired, lifecycle.ErrPlanChanged,
		lifecycle.ErrNotResumable, lifecycle.ErrOpRunning,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return RefusalNote(err) != ""
}

func (s *Service) CurrentVersion(ctx context.Context, id string) Version {
	snap, err := s.db.GetIncidentSnapshot(ctx, id)
	if err != nil {
		return Version{}
	}
	return Version{N: snap.Version, Set: true}
}

func (s *Service) Actions(ctx context.Context, snap lifecycle.Snapshot) []lifecycle.Action {
	snap.In.Limits = s.limits(s.timers(ctx))
	return lifecycle.Actions(snap, time.Now())
}

func (s *Service) SystemRerun(ctx context.Context, id string, gen int64) error {
	leave, err := s.ownerSection(verbRerun, id)
	if err != nil {
		return err
	}
	defer leave()
	_, err = s.apply(ctx, id, lifecycle.Event{Kind: lifecycle.SystemRerun, Gen: gen}, nil)
	return err
}

func (s *Service) ownerRerunEvents(ctx context.Context, id string) eventsFor {
	return func(_ lifecycle.Snapshot, d lifecycle.Decision) []*db.Event {
		marker, err := journal.OwnerRerunEvent(id, d.Next.Generation)
		if err != nil {
			s.log.ErrorContext(ctx, "build owner rerun event", "incident", id, "error", err)
			return nil
		}
		return []*db.Event{marker}
	}
}
