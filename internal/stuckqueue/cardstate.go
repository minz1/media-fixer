package stuckqueue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/operation"
)

const (
	cardKind     = "sq"
	cardPrefix   = cardKind + ":"
	cardArgs     = 3
	subjectParts = 3
	verbResume   = "resume"
	refusedLine  = "Refused: "
	goneLine     = "left the queue"
)

func actedLine(a Acted) string {
	return fmt.Sprintf("✅ %s · <t:%d:t>", actionDone(a.Action), a.At.Unix())
}

func actionDone(a Action) string {
	switch a {
	case ActionImport:
		return "Imported"
	case ActionRegrab:
		return "Re-grabbed"
	case ActionRemove:
		return "Removed"
	}
	return string(a)
}

func appCode(a App) string {
	switch a {
	case AppSonarr:
		return "s"
	case AppRadarr:
		return "r"
	}
	return ""
}

func AppForCode(code string) (App, bool) { return appOf(code) }

func appOf(code string) (App, bool) {
	for _, a := range []App{AppSonarr, AppRadarr} {
		if appCode(a) == code {
			return a, true
		}
	}
	return "", false
}

func subjectOf(d Download) string {
	return card.StuckSubject(appCode(d.App), d.DownloadID)
}

func itemButtons(d Download, actions []Action) []card.Button {
	var out []card.Button
	for _, a := range actions {
		id, ok := card.Encode(card.Action{
			Kind: cardKind, Verb: string(a), Args: []string{appCode(d.App), d.DownloadID, d.TargetsFP()},
		})
		if !ok {
			continue
		}
		label, style := buttonLook(a)
		out = append(out, card.Button{Label: label, Style: style, ID: id})
	}
	return out
}

func buttonLook(a Action) (string, card.Style) {
	switch a {
	case ActionImport:
		return "Import", card.Primary
	case ActionRegrab:
		return "Re-grab", card.Secondary
	case ActionRemove:
		return "Remove", card.Secondary
	}
	return string(a), card.Secondary
}

func (s *Scheduler) CardRenderer() card.Renderer {
	return func(ctx context.Context, subject string) (card.Rendering, error) {
		parts := strings.SplitN(subject, ":", subjectParts)
		if len(parts) != subjectParts || parts[0] != cardKind {
			return card.Rendering{Outcome: card.Gone}, nil
		}
		app, ok := appOf(parts[1])
		if !ok {
			return card.Rendering{Outcome: card.Gone}, nil
		}
		done, hasDone, err := s.actedRow(ctx, app, parts[2])
		if err != nil {
			return card.Rendering{}, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if op, failed := s.slot.failedOps[OpSubject(app, parts[2]).ID]; failed {
			return card.Rendered(failedUpdate(app, op)), nil
		}
		if hasDone {
			return card.Rendered(card.Update{Status: actedLine(done)}), nil
		}
		return s.renderLast(app, parts[2]), nil
	}
}

func (s *Scheduler) renderLast(app App, downloadID string) card.Rendering {
	if !s.slot.hasLast {
		return card.Rendering{}
	}
	i := slices.IndexFunc(s.slot.last.Apps, func(ar AppResult) bool { return ar.App == app })
	if i < 0 || !s.slot.last.Apps[i].Configured || s.slot.last.Apps[i].Unreachable {
		return card.Rendering{}
	}
	ar := s.slot.last.Apps[i]
	match := func(it Item) bool { return strings.EqualFold(it.DownloadID, downloadID) }
	if j := slices.IndexFunc(ar.Items, match); j >= 0 {
		return card.Rendered(card.Update{Buttons: itemButtons(ar.Items[j].Download, ar.Items[j].Actions)})
	}
	if j := slices.IndexFunc(ar.Waiting, match); j >= 0 {
		w := ar.Waiting[j]
		return card.Rendered(card.Update{
			Status:  fmt.Sprintf("⏳ Inside the wait until <t:%d:t>; only Remove until then", w.Due.Unix()),
			Buttons: itemButtons(w.Download, w.Actions),
		})
	}
	if s.slot.holder == holderRun {
		return card.Rendering{}
	}
	return card.Rendered(card.Update{Status: goneLine})
}

func (s *Scheduler) CardHandler() card.Handler {
	return func(ctx context.Context, a card.Action) (card.Result, error) {
		if len(a.Args) != cardArgs {
			return card.Result{}, fmt.Errorf("%w: %d args", card.ErrUnknownID, len(a.Args))
		}
		app, ok := appOf(a.Args[0])
		if !ok {
			return card.Result{}, fmt.Errorf("%w: app %q", card.ErrUnknownID, a.Args[0])
		}
		var notice string
		var err error
		if a.Verb == verbResume {
			notice, err = s.resumeFrom(ctx, app, a.Args[1], a.Args[2])
		} else {
			notice, err = s.Act(ctx, app, a.Args[1], Action(a.Verb), a.Args[2])
		}

		if errors.Is(err, ErrRefused) {
			return card.Result{Line: refusedLine + strings.TrimPrefix(err.Error(), ErrRefused.Error()+": ")}, nil
		}
		if err != nil {
			return card.Result{}, err
		}
		return card.Result{Line: notice}, nil
	}
}

func (s *Scheduler) actedRow(ctx context.Context, app App, downloadID string) (Acted, bool, error) {
	rows, err := s.d.ListStuckInState(ctx, string(StateActed))
	if err != nil {
		return Acted{}, false, err
	}
	for _, row := range rows {
		if App(row.App) == app && strings.EqualFold(row.DownloadID, downloadID) && row.ActedResult == string(ResultOK) {
			return *fromRow(row).Acted, true, nil
		}
	}
	return Acted{}, false, nil
}

func (s *Scheduler) refresh(subject string) {
	if s.Refresh != nil {
		s.Refresh(subject)
	}
}

func failedUpdate(app App, op operation.Op) card.Update {
	up := card.Update{Status: "⚠️ " + operation.Line(op) + causeSuffix(op.Cause)}
	id, ok := card.Encode(card.Action{
		Kind: cardKind, Verb: verbResume,
		Args: []string{appCode(app), op.Steps[0].DownloadID, fmt.Sprintf("o%d.%d", op.ID, op.Version)},
	})
	if ok {
		up.Buttons = []card.Button{{Label: "Resume", Style: card.Secondary, ID: id}}
	}
	return up
}

func causeSuffix(c operation.Cause) string {
	switch c {
	case operation.CauseStore:
		return " (database error)"
	case operation.CauseRestart:
		return " (restart)"
	case operation.CauseNone, operation.CauseError:
	}
	return ""
}

func (s *Scheduler) resumeFrom(ctx context.Context, app App, downloadID, ref string) (string, error) {
	idPart, verPart, ok := strings.Cut(strings.TrimPrefix(ref, "o"), ".")
	opID, idErr := strconv.ParseInt(idPart, 10, 64)
	opVersion, verErr := strconv.ParseInt(verPart, 10, 64)
	if !strings.HasPrefix(ref, "o") || !ok || idErr != nil || verErr != nil {
		return "", fmt.Errorf("%w: operation %q", card.ErrUnknownID, ref)
	}
	return s.Resume(ctx, app, downloadID, opID, opVersion)
}
