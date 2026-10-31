package incident

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	kindIncident = "inc"
	verbConfirm  = "confirm"
	verbCancel   = "cancel"
	planMaxLines = 15
	planTooLong  = "Plan too long for Discord; review it on the dashboard"
	refusedLine  = "Refused: "
	minIncArgs   = 2
	fpArgs       = 3
)

type cardPlan struct {
	sections []card.Field
	fp       string
	fits     bool
}

func (s *Service) CardRenderer() card.Renderer {
	return func(ctx context.Context, subject string) (card.Update, bool, error) {
		id := strings.TrimPrefix(subject, card.IncidentSubject(""))
		inc, err := s.db.GetIncident(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return card.Update{}, false, nil
		}
		if err != nil {
			return card.Update{}, false, err
		}
		up, err := s.cardUpdate(ctx, inc)
		return up, err == nil, err
	}
}

type buttonSpec struct {
	label string
	style card.Style
	verb  string
	extra []string
}

func (s *Service) cardUpdate(ctx context.Context, inc *db.Incident) (card.Update, error) {
	up := card.Update{Status: statusLine(inc.Status)}
	specs, sections, err := s.escalationSpecs(ctx, inc)
	if err != nil {
		return card.Update{}, err
	}
	up.Sections = sections
	specs = append(specs, buttonSpec{"Rerun", card.Secondary, verbRerun, nil})
	if inc.Status != db.StatusResolved {
		specs = append(specs, buttonSpec{"Resolve", card.Primary, verbResolve, nil})
	}
	if inc.Status == db.StatusManualTestNeeded {
		_, err = s.db.GetPendingOutcome(ctx, inc.ID)
		switch {
		case err == nil:
			specs = append(specs, buttonSpec{"Keep searching", card.Primary, verbKeep, nil})
		case !errors.Is(err, db.ErrNotFound):
			return card.Update{}, err
		}
	}
	if inc.AutonomousLocked {
		specs = append(specs, buttonSpec{"Unlock", card.Secondary, verbUnlock, nil})
	}

	for _, sp := range specs {
		args := append([]string{inc.ID, string(inc.Status)}, sp.extra...)
		if id, ok := card.Encode(card.Action{Kind: kindIncident, Verb: sp.verb, Args: args}); ok {
			up.Buttons = append(up.Buttons, card.Button{Label: sp.label, Style: sp.style, ID: id})
		}
	}
	return up, nil
}

func (s *Service) escalationSpecs(ctx context.Context, inc *db.Incident) ([]buttonSpec, []card.Field, error) {
	action := escalatable(inc)
	if action == "" {
		return nil, nil, nil
	}
	plan, fresh, err := s.freshPlan(ctx, inc, action)
	if err != nil || !fresh {
		return []buttonSpec{{"Preview plan", card.Primary, verbPreview, nil}}, nil, err
	}
	var specs []buttonSpec
	if plan.fits {
		specs = append(specs, buttonSpec{"Approve", card.Danger, verbApprove, []string{plan.fp}})
	}
	return append(specs, buttonSpec{"Refresh preview", card.Secondary, verbRefresh, nil}), plan.sections, nil
}

func statusLine(st db.IncidentStatus) string {
	switch st {
	case db.StatusOpen:
		return "🆕 Open"
	case db.StatusInvestigating:
		return "🔁 Investigating…"
	case db.StatusVerifying:
		return "⏳ Verifying the fix"
	case db.StatusReopened:
		return "🔁 Re-opened"
	case db.StatusBlocked:
		return "⛔ Blocked"
	case db.StatusAgentFixed:
		return "✅ Fixed by the agent"
	case db.StatusManualTestNeeded:
		return "🧑‍🔧 Needs you"
	case db.StatusResolved:
		return "✅ Resolved"
	}
	return string(st)
}

func escalatable(inc *db.Incident) string {
	if inc.Status != db.StatusManualTestNeeded {
		return ""
	}
	result, err := escalationResult(inc)
	if err != nil {
		return ""
	}
	switch result.EscalateAction {
	case agent.EscalateRemoveAndSearch, agent.EscalateDeleteTorrentReadd:
		return result.EscalateAction
	}
	return ""
}

func (s *Service) freshPlan(ctx context.Context, inc *db.Incident, action string) (cardPlan, bool, error) {
	planJSON, at, err := s.db.GetEscalationPlan(ctx, inc.ID)
	if errors.Is(err, db.ErrNoEscalationPlan) {
		return cardPlan{}, false, nil
	}
	if err != nil {
		return cardPlan{}, false, err
	}
	expires := at.Add(escalationPlanTTL)
	if time.Now().After(expires) {
		return cardPlan{}, false, nil
	}
	var secs []card.Field
	var lines int
	if action == agent.EscalateDeleteTorrentReadd {
		var p client.ReaddPlan
		if err = json.Unmarshal(planJSON, &p); err != nil {
			return cardPlan{}, false, fmt.Errorf("decode readd plan: %w", err)
		}
		secs = []card.Field{
			{Name: "Torrent", Value: p.Name},
			{Name: "Provider", Value: p.Provider},
			{Name: "State", Value: p.State},
		}
		lines = 1
	} else {
		var p client.ReplacePlan
		if err = json.Unmarshal(planJSON, &p); err != nil {
			return cardPlan{}, false, fmt.Errorf("decode replace plan: %w", err)
		}
		secs = replaceSections(&p, inc.What)
		lines = len(p.Files)
	}
	secs = append(secs, card.Field{Name: "Expires", Value: fmt.Sprintf("<t:%d:t>", expires.Unix())})
	plan := cardPlan{
		sections: secs,
		fp:       card.Fingerprint(planJSON),
		fits:     lines <= planMaxLines && sectionsLen(secs) <= card.MaxText,
	}
	if !plan.fits {
		plan.sections = []card.Field{{Name: "Plan", Value: planTooLong}}
	}
	return plan, true, nil
}

func replaceSections(p *client.ReplacePlan, what string) []card.Field {
	readability := "Unknown: no media-agent tested these files"
	if what == agent.WhatStuckRead {
		readability = "Not probed: reading a stuck file would hang"
	}
	switch r := p.Readability; {
	case r == nil || !r.Checked:
	case r.ReadableFile != "":
		readability = "⚠️ A file this plan would delete currently reads fine: `" + r.ReadableFile + "`"
	default:
		readability = fmt.Sprintf("None of the %d files probed read fine", r.Probed)
	}
	secs := []card.Field{{Name: "Readability", Value: readability}, {Name: "Reason", Value: p.Reason}}
	for _, f := range p.Files {
		secs = append(secs, card.Field{Name: "Delete", Value: "`" + f.Path + "`"})
	}
	then := fmt.Sprintf("Blocklist %d grabs and search again", len(p.GrabsToBlocklist))
	if p.SkipBlocklist {
		then = "Search again without blocklisting"
	}
	return append(secs, card.Field{Name: "Then", Value: then})
}

func sectionsLen(secs []card.Field) int {
	n := 0
	for _, f := range secs {
		n += len("**:** \n") + len(f.Name) + len(f.Value)
	}
	return n
}

func (s *Service) CardHandler() card.Handler {
	return func(ctx context.Context, a card.Action) (card.Result, error) {
		if a.Verb == verbCancel {
			return card.Result{}, nil
		}
		if len(a.Args) < minIncArgs {
			return card.Result{}, fmt.Errorf("%w: %s %v", card.ErrUnknownID, a.Verb, a.Args)
		}
		id := a.Args[0]
		inc, err := s.db.GetIncident(ctx, id)
		if err != nil {
			return refusal(err)
		}
		if string(inc.Status) != a.Args[1] {
			return card.Result{
				Line: refusedLine + "this incident is now " + strings.ReplaceAll(string(inc.Status), "_", " "),
			}, nil
		}
		res, err := s.cardAction(ctx, id, a)
		if err != nil {
			return refusal(err)
		}
		return res, nil
	}
}

func (s *Service) cardAction(ctx context.Context, id string, a card.Action) (card.Result, error) {
	done := func(line string) (card.Result, error) {
		return card.Result{Line: fmt.Sprintf("%s · <t:%d:t>", line, time.Now().Unix())}, nil
	}
	switch a.Verb {
	case verbResolve:
		if err := s.Resolve(ctx, id); err != nil {
			return card.Result{}, err
		}
		return done("✅ Resolved")
	case verbRerun:
		if err := s.Rerun(ctx, id); err != nil {
			return card.Result{}, err
		}
		return done("🔁 Re-running")
	case verbKeep:
		if err := s.KeepSearching(ctx, id); err != nil {
			return card.Result{}, err
		}
		return done("🔎 Keeping the search going")
	case verbUnlock:
		if err := s.Unlock(ctx, id); err != nil {
			return card.Result{}, err
		}
		return done("🔓 Unlocked")
	case verbPreview, verbRefresh:
		if _, _, err := s.PreviewEscalation(ctx, id); err != nil {
			return card.Result{}, err
		}
		return done("📋 Plan ready")
	case verbApprove, verbConfirm:
		if len(a.Args) < fpArgs {
			return card.Result{}, fmt.Errorf("%w: %s %v", card.ErrUnknownID, a.Verb, a.Args)
		}
		return s.cardApprove(ctx, id, a)
	}
	return card.Result{}, fmt.Errorf("%w: verb %s", card.ErrUnknownID, a.Verb)
}

func (s *Service) cardApprove(ctx context.Context, id string, a card.Action) (card.Result, error) {
	fp := a.Args[fpArgs-1]
	if a.Verb == verbConfirm {
		if err := s.ApproveEscalation(ctx, id, fp); err != nil {
			return card.Result{}, err
		}
		return card.Result{Line: fmt.Sprintf("🚀 Approved · <t:%d:t>", time.Now().Unix())}, nil
	}
	_, result, _, err := s.approvable(ctx, id, fp)
	if err != nil {
		return card.Result{}, err
	}
	label := "Yes, delete and re-search"
	if result.EscalateAction == agent.EscalateDeleteTorrentReadd {
		label = "Yes, delete and re-add"
	}
	confirm, okConfirm := card.Encode(card.Action{Kind: a.Kind, Verb: verbConfirm, Args: a.Args})
	cancel, okCancel := card.Encode(card.Action{Kind: a.Kind, Verb: verbCancel, Args: a.Args[:minIncArgs]})
	if !okConfirm || !okCancel {
		return card.Result{}, fmt.Errorf("%w: confirm id too long", card.ErrUnknownID)
	}
	return card.Result{Confirm: []card.Button{
		{Label: label, Style: card.Danger, ID: confirm},
		{Label: "Cancel", Style: card.Secondary, ID: cancel},
	}}, nil
}

func refusal(err error) (card.Result, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return card.Result{Line: refusedLine + "this incident no longer exists"}, nil
	}
	for _, target := range []error{
		errNotAwaitingDecision, errNoPlan, errPlanExpired, errPlanChanged, ErrAlreadyResolved, db.ErrNotFound,
	} {
		if errors.Is(err, target) {
			return card.Result{Line: refusedLine + err.Error()}, nil
		}
	}
	return card.Result{}, err
}
