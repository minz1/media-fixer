package incident

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/incident/lifecycle"
	"github.com/minz1/mediafixer/internal/operation"
)

const (
	kindIncident = "inc"
	verbConfirm  = "confirm"
	verbCancel   = "cancel"
	verbResume   = "resume"
	planMaxLines = 15
	planTooLong  = "Plan too long for Discord; review it on the dashboard"
	refusedLine  = "Refused: "
	minIncArgs   = 2
	fpArgs       = 3
	opArgs       = 3
)

type cardPlan struct {
	sections []card.Field
	fp       string
	fits     bool
}

func (s *Service) CardRenderer() card.Renderer {
	return func(ctx context.Context, subject string) (card.Rendering, error) {
		id := strings.TrimPrefix(subject, card.IncidentSubject(""))
		inc, snap, err := s.db.IncidentCardView(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return card.Rendering{Outcome: card.Gone}, nil
		}
		if err != nil {
			return card.Rendering{}, err
		}
		up, err := s.cardUpdate(ctx, inc, snap)
		if err != nil {
			return card.Rendering{}, err
		}
		return card.Rendered(up), nil
	}
}

type buttonSpec struct {
	label string
	style card.Style
	verb  string
	extra []string
	kind  lifecycle.ActionKind
}

func (s *Service) cardUpdate(ctx context.Context, inc *db.Incident, snap lifecycle.Snapshot) (card.Update, error) {
	var err error
	snap.In.Limits = s.limits(s.timers(ctx))
	up := card.Update{Status: statusLine(inc.Status) + statusNotes(snap)}
	var plan cardPlan
	if escalatable(inc) && snap.PlanFresh(time.Now()) {
		if plan, _, err = s.freshPlan(ctx, inc); err != nil {
			return card.Update{}, err
		}
		up.Sections = plan.sections
	}
	if line := s.PartialLine(ctx, snap); line != "" {
		up.Sections = append(up.Sections, card.Field{Name: "Result", Value: line})
	}
	acts := lifecycle.Actions(snap, time.Now())
	v := Version{N: snap.Version, Set: true}
	up.Sections = append(up.Sections, nextSection(snap, acts)...)
	for _, sp := range cardSpecs(plan, snap.Op) {
		if !slices.ContainsFunc(acts, func(a lifecycle.Action) bool { return a.Kind == sp.kind }) ||
			(sp.kind == lifecycle.ActApprove && !plan.fits) {
			continue
		}
		args := append([]string{inc.ID, v.Token()}, sp.extra...)
		if id, ok := card.Encode(card.Action{Kind: kindIncident, Verb: sp.verb, Args: args}); ok {
			up.Buttons = append(up.Buttons, card.Button{Label: sp.label, Style: sp.style, ID: id})
		}
	}
	return up, nil
}

func cardSpecs(plan cardPlan, op *operation.Op) []buttonSpec {
	var resume []string
	if op != nil {
		resume = []string{opToken(op.ID, op.Version)}
	}
	return []buttonSpec{
		{"Preview plan", card.Primary, verbPreview, nil, lifecycle.ActPreview},
		{"Approve", card.Danger, verbApprove, []string{plan.fp}, lifecycle.ActApprove},
		{"Resume", card.Secondary, verbResume, resume, lifecycle.ActResume},
		{"Refresh preview", card.Secondary, verbRefresh, nil, lifecycle.ActRefresh},
		{"Rerun", card.Secondary, verbRerun, nil, lifecycle.ActRerun},
		{"Resolve", card.Primary, verbResolve, nil, lifecycle.ActResolve},
		{"Keep searching", card.Primary, verbKeep, nil, lifecycle.ActKeepSearching},
		{"Unlock", card.Secondary, verbUnlock, nil, lifecycle.ActUnlock},
	}
}

func BudgetSpent(s lifecycle.Snapshot) bool {
	return s.Locked && s.ActionCount >= agent.MaxAutonomousActions
}

func statusNotes(s lifecycle.Snapshot) string {
	var out string
	if s.Parked {
		out += " · paused"
	}
	if BudgetSpent(s) {
		out += " · budget spent"
	}
	return out
}

func (s *Service) PartialLine(ctx context.Context, snap lifecycle.Snapshot) string {
	if snap.Reason != lifecycle.ReasonPlanPartial {
		return ""
	}
	actions, err := s.db.ListActions(ctx, snap.ID)
	if err != nil {
		return ""
	}
	for _, a := range slices.Backward(actions) {
		if a.TriggeredBy == "owner" && a.Status == db.ActionFailed {
			var result any
			if a.Result != "" {
				result = json.RawMessage(a.Result)
			}
			return partialOf(a.Action, result)
		}
	}
	return ""
}

func statusLine(st db.IncidentStatus) string {
	if line := lifecycle.Meta(st).Line; line != "" {
		return line
	}
	return string(st)
}

func escalatable(inc *db.Incident) bool {
	if inc.Status != db.StatusManualTestNeeded {
		return false
	}
	result, err := escalationResult(inc)
	return err == nil && result.EscalateAction == agent.EscalateRemoveAndSearch
}

func (s *Service) freshPlan(ctx context.Context, inc *db.Incident) (cardPlan, bool, error) {
	planJSON, at, err := s.db.GetEscalationPlan(ctx, inc.ID)
	if errors.Is(err, db.ErrNoEscalationPlan) {
		return cardPlan{}, false, nil
	}
	if err != nil {
		return cardPlan{}, false, err
	}
	expires := at.Add(s.minutes(planTTLMinutes))
	if time.Now().After(expires) {
		return cardPlan{}, false, nil
	}
	var p client.ReplacePlan
	if err = json.Unmarshal(planJSON, &p); err != nil {
		return cardPlan{}, false, fmt.Errorf("decode replace plan: %w", err)
	}
	secs := replaceSections(&p, inc.What)
	lines := len(p.Files)
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
	secs := []card.Field{{Name: "Readability", Value: readability}, {Name: fieldReason, Value: p.Reason}}
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
		if _, err := s.db.GetIncident(ctx, id); err != nil {
			return refusal(err)
		}
		res, err := s.cardAction(ctx, id, ParseVersion(a.Args[1]), a)
		if err != nil {
			return refusal(err)
		}
		return res, nil
	}
}

func (s *Service) cardAction(ctx context.Context, id string, v Version, a card.Action) (card.Result, error) {
	done := func(line string) (card.Result, error) {
		return card.Result{Line: fmt.Sprintf("%s · <t:%d:t>", line, time.Now().Unix())}, nil
	}
	switch a.Verb {
	case verbResolve:
		if err := s.Resolve(ctx, id, v); err != nil {
			return card.Result{}, err
		}
		return done("✅ Resolved")
	case verbRerun:
		if err := s.Rerun(ctx, id, v); err != nil {
			return card.Result{}, err
		}
		return done("🔁 Re-running")
	case verbKeep:
		if err := s.KeepSearching(ctx, id, v); err != nil {
			return card.Result{}, err
		}
		return done("🔎 Keeping the search going")
	case verbUnlock:
		if err := s.Unlock(ctx, id, v); err != nil {
			return card.Result{}, err
		}
		return done("🔓 Unlocked")
	case verbResume:
		return s.cardResume(ctx, id, v, a)
	case verbPreview, verbRefresh:
		if _, _, err := s.PreviewEscalation(ctx, id, v); err != nil {
			return card.Result{}, err
		}
		return done("📋 Plan ready")
	case verbApprove, verbConfirm:
		if len(a.Args) < fpArgs {
			return card.Result{}, fmt.Errorf("%w: %s %v", card.ErrUnknownID, a.Verb, a.Args)
		}
		return s.cardApprove(ctx, id, v, a)
	}
	return card.Result{}, fmt.Errorf("%w: verb %s", card.ErrUnknownID, a.Verb)
}

func (s *Service) cardApprove(ctx context.Context, id string, v Version, a card.Action) (card.Result, error) {
	fp := a.Args[fpArgs-1]
	if a.Verb == verbConfirm {
		if err := s.ApproveEscalation(ctx, id, fp, v); err != nil {
			return card.Result{}, err
		}
		return card.Result{Line: fmt.Sprintf("🚀 Approved · <t:%d:t>", time.Now().Unix())}, nil
	}
	_, _, err := s.approvable(ctx, id, fp)
	if err != nil {
		return card.Result{}, err
	}
	confirm, okConfirm := card.Encode(card.Action{Kind: a.Kind, Verb: verbConfirm, Args: a.Args})
	cancel, okCancel := card.Encode(card.Action{Kind: a.Kind, Verb: verbCancel, Args: a.Args[:minIncArgs]})
	if !okConfirm || !okCancel {
		return card.Result{}, fmt.Errorf("%w: confirm id too long", card.ErrUnknownID)
	}
	return card.Result{Confirm: []card.Button{
		{Label: "Yes, delete and re-search", Style: card.Danger, ID: confirm},
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
		if errors.Is(err, target) || IsRefusal(err) {
			return card.Result{Line: refusedLine + err.Error()}, nil
		}
	}
	return card.Result{}, err
}

func opToken(opID, opVersion int64) string { return fmt.Sprintf("o%d.%d", opID, opVersion) }

func resumeArgs(id string, v Version, opID, opVersion int64) []string {
	return []string{id, v.Token(), opToken(opID, opVersion)}
}

func parseOpToken(s string) (int64, int64, bool) {
	rest, ok := strings.CutPrefix(s, "o")
	if !ok {
		return 0, 0, false
	}
	idPart, verPart, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, 0, false
	}
	opID, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	opVersion, err := strconv.ParseInt(verPart, 10, 64)
	return opID, opVersion, err == nil
}

func nextSection(snap lifecycle.Snapshot, acts []lifecycle.Action) []card.Field {
	if snap.Op == nil ||
		!slices.ContainsFunc(acts, func(a lifecycle.Action) bool { return a.Kind == lifecycle.ActResume }) {
		return nil
	}
	return []card.Field{{Name: "Next", Value: operation.NextLine(*snap.Op)}}
}

func (s *Service) cardResume(ctx context.Context, id string, v Version, a card.Action) (card.Result, error) {
	if len(a.Args) < opArgs {
		return card.Result{}, fmt.Errorf("%w: %s %v", card.ErrUnknownID, a.Verb, a.Args)
	}
	opID, opVersion, ok := parseOpToken(a.Args[opArgs-1])
	if !ok {
		return card.Result{}, fmt.Errorf("%w: %s %v", card.ErrUnknownID, a.Verb, a.Args)
	}
	if err := s.ResumeOperation(ctx, id, v, opID, opVersion); err != nil {
		return card.Result{}, err
	}
	return card.Result{Line: fmt.Sprintf("▶️ Resumed · <t:%d:t>", time.Now().Unix())}, nil
}
