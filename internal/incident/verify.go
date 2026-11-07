package incident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
)

type Observer interface {
	VerifyResolved(ctx context.Context, itemID, title string, pre *agent.FixSignature) bool
	ScanRunning(ctx context.Context) bool
	CheckPendingOutcome(ctx context.Context, po *db.PendingOutcome) (*agent.PendingOutcomeObservation, error)
}

func (s *Service) observer() Observer {
	if s.agent == nil {
		return nil
	}
	return s.agent
}

func (s *Service) runVerification(ctx context.Context, inc *db.Incident, gen int64, result *agent.DiagnosticResult) {
	itemID := inc.JellyfinItemID
	if itemID == "" {
		itemID = result.VerifyItemID
	}
	spec := lifecycle.VerifySpec{
		Kind: lifecycle.VerifyGeneric, ItemID: itemID, Action: result.PrimaryAction, ETAMinutes: result.UserETAMinutes,
	}
	if inc.What == agent.WhatStuckRead {
		spec.Kind = lifecycle.VerifyStuckRead
	}
	if result.PreFix != nil {
		spec.PreFix, _ = json.Marshal(result.PreFix)
	}
	ev := lifecycle.Event{Kind: lifecycle.RunFinished, Gen: gen, Outcome: lifecycle.Outcome{
		Kind: lifecycle.OutcomeVerify, Spec: spec, After: time.Duration(result.VerifyAfterSeconds) * time.Second,
	}}
	s.applyOutcome(ctx, inc.ID, ev)
}

func (s *Service) applyOutcome(ctx context.Context, id string, ev lifecycle.Event) {
	if ctx.Err() != nil {
		s.log.InfoContext(
			ctx,
			"run outcome dropped, the run was stopped",
			"incident",
			id,
			"outcome",
			string(ev.Outcome.Kind),
		)
		return
	}
	_, err := s.apply(ctx, id, ev, nil)
	switch {
	case err == nil:
	case errors.Is(err, lifecycle.ErrStale), errors.Is(err, lifecycle.ErrUnclaimed):
		s.log.InfoContext(ctx, "run outcome not applied, incident already moved on",
			"incident", id, "outcome", string(ev.Outcome.Kind), "error", err)
	default:
		s.log.ErrorContext(ctx, "apply run outcome", "incident", id, "outcome", string(ev.Outcome.Kind), "error", err)
	}
}

func (s *Service) runCheck(ctx context.Context, id string, gen int64, kind lifecycle.CheckKind) {
	switch kind {
	case lifecycle.CheckVerify:
		s.verifyCheck(ctx, id, gen)
	case lifecycle.CheckDownload:
		s.downloadCheck(ctx, id, gen)
	}
}

func (s *Service) checkTarget(ctx context.Context, id string, gen int64, want lifecycle.State) (
	lifecycle.Snapshot, *db.Incident, bool,
) {
	snap, err := s.db.GetIncidentSnapshot(ctx, id)
	if err != nil {
		s.log.ErrorContext(ctx, "check: load incident", "incident", id, "error", err)
		return snap, nil, false
	}
	if snap.Generation != gen || snap.Status != want {
		s.log.InfoContext(ctx, "check stale, incident moved on", "incident", id, "gen", gen,
			"current", snap.Generation, "status", string(snap.Status))
		return snap, nil, false
	}
	inc, err := s.db.GetIncident(ctx, id)
	if err != nil {
		s.log.ErrorContext(ctx, "check: load incident", "incident", id, "error", err)
		return snap, nil, false
	}
	return snap, inc, true
}

func (s *Service) verifyCheck(ctx context.Context, id string, gen int64) {
	snap, inc, ok := s.checkTarget(ctx, id, gen, lifecycle.Verifying)
	if !ok || snap.Verify == nil {
		return
	}
	res, scan := s.observeVerify(ctx, inc, snap)
	ev := lifecycle.Event{Kind: lifecycle.VerifyObserved, Gen: gen, Verify: res, Scan: scan, Notice: s.notVerified(inc)}
	if _, err := s.apply(ctx, id, ev, nil); err != nil {
		s.logCheckErr(ctx, id, err)
	}
}

func (s *Service) logCheckErr(ctx context.Context, id string, err error) {
	if errors.Is(err, lifecycle.ErrStale) {
		s.log.InfoContext(ctx, "check stale, incident moved on", "incident", id)
		return
	}
	if ctx.Err() != nil {
		s.log.InfoContext(ctx, "check result dropped, the check was stopped", "incident", id, "error", err)
		return
	}
	s.log.ErrorContext(ctx, "apply check result", "incident", id, "error", err)
}

func (s *Service) observeVerify(ctx context.Context, inc *db.Incident, snap lifecycle.Snapshot) (
	lifecycle.VerifyResult, bool,
) {
	spec := snap.Verify.Spec
	if spec.Kind == lifecycle.VerifyStuckRead {
		switch {
		case s.stuckRead == nil:
			return lifecycle.VerifyUnknown, false
		case s.stuckRead.Clear(inc.Title):
			return lifecycle.VerifyFixed, false
		}
		return lifecycle.VerifyNotYet, false
	}
	obs := s.observer()
	if obs == nil {
		return lifecycle.VerifyUnknown, false
	}
	var pre *agent.FixSignature
	if len(spec.PreFix) > 0 {
		_ = json.Unmarshal(spec.PreFix, &pre)
	}
	if obs.VerifyResolved(ctx, spec.ItemID, inc.Title, pre) {
		return lifecycle.VerifyFixed, false
	}
	if snap.Verify.ChecksLeft > 1 {
		return lifecycle.VerifyNotYet, false
	}
	return lifecycle.VerifyNotYet, obs.ScanRunning(ctx)
}

func (s *Service) notVerified(inc *db.Incident) lifecycle.Notice {
	fields := []card.Field{}
	if result, ok := agent.ParseDiagnosticResult(inc.Finding); ok {
		fields = append(fields,
			card.Field{Name: fieldRootCause, Value: result.RootCause},
			card.Field{Name: fieldAction, Value: agent.EscalationSummary(result)},
			card.Field{Name: fieldReason, Value: result.PrimaryReason})
	}
	return noticeOf(ownerCard(inc, card.Warn, fmt.Sprintf(
		"🔍 Incident **%s** (#%s) needs your attention (autonomous fix applied but could not be verified).",
		inc.Title, inc.ShortID()), "", fields...))
}
