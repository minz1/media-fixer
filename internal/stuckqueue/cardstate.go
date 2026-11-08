package stuckqueue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/operation"
)

const (
	cardKind       = "sq"
	cardPrefix     = cardKind + ":"
	cardArgs       = 3
	subjectParts   = 3
	verbResume     = "resume"
	refusedLine    = "Refused: "
	goneLine       = "left the queue"
	unfinishedLine = "⚠️ Re-grab didn't finish"
)

func actedLine(a Acted) string {
	name := actionDone(a.Action)
	if a.Action == ActionImport && a.Files.M > 0 {
		name = fmt.Sprintf("%s %d of %d files", name, a.Files.N, a.Files.M)
	}
	return fmt.Sprintf("✅ %s · <t:%d:t>", name, a.At.Unix())
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
	return buttonsFor(Key{App: d.App, DownloadID: d.DownloadID}, d.TargetsFP(), actions)
}

func buttonsFor(key Key, fp string, actions []Action) []card.Button {
	var out []card.Button
	for _, a := range actions {
		id, ok := card.Encode(card.Action{
			Kind: cardKind, Verb: string(a), Args: []string{appCode(key.App), key.DownloadID, fp},
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

func (s *Scheduler) refresh(subject string) {
	if s.Refresh != nil {
		s.Refresh(subject)
	}
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
		rows, err := s.d.ListStuck(ctx, string(app))
		if err != nil {
			return card.Rendering{}, err
		}
		i := slices.IndexFunc(rows, func(r db.StuckRow) bool { return strings.EqualFold(r.DownloadID, parts[2]) })
		if i < 0 {
			return card.Rendered(card.Update{Status: goneLine}), nil
		}
		return s.renderRow(ctx, fromRow(rows[i]))
	}
}

func (s *Scheduler) renderRow(ctx context.Context, snap Snapshot) (card.Rendering, error) {
	switch snap.State {
	case StateUntracked, StateCleared:
		return card.Rendered(card.Update{Status: goneLine}), nil
	case StateImportInflight:
		status := fmt.Sprintf("⏳ Importing since <t:%d:t>", snap.Import.StartedAt.Unix())
		return card.Rendered(card.Update{Status: status}), nil
	case StateActed:
		return s.renderActed(ctx, snap)
	case StateWaiting:
		set, err := LoadSettings(ctx, s.d)
		if err != nil {
			return card.Rendering{}, err
		}
		return card.Rendered(storedUpdate(snap, waitLine(snap, s.r.Now(), set.MinAge()), s.r.Now(), set.MinAge())), nil
	case StateDue, StateHeld, StateNotified:
		return card.Rendered(storedUpdate(snap, noteLine(snap.Acted), s.r.Now(), 0)), nil
	}
	return card.Rendering{Outcome: card.Gone}, nil
}

func storedUpdate(snap Snapshot, status string, now time.Time, minAge time.Duration) card.Update {
	return card.Update{
		Status:  status,
		Buttons: buttonsFor(snap.Key, snap.TargetsFP, Actions(snap, now, minAge)),
	}
}

func waitLine(snap Snapshot, now time.Time, minAge time.Duration) string {
	due := snap.FirstSeen.Add(minAge)
	if !now.Before(due) {
		return ""
	}
	return fmt.Sprintf("⏳ until <t:%d:t>", due.Unix())
}

func noteLine(a *Acted) string {
	if a == nil {
		return ""
	}
	switch a.Result {
	case ResultImportFailed:
		return "⚠️ Import failed"
	case ResultImportTimeout:
		return "⚠️ Import still running"
	case ResultImportUnconfirmed:
		return "⚠️ Import not confirmed"
	case ResultPartial:
		return fmt.Sprintf("⚠️ Imported %d of %d files", a.Files.N, a.Files.M)
	case ResultOK, ResultUnfinished:
	}
	return ""
}

func (s *Scheduler) renderActed(ctx context.Context, snap Snapshot) (card.Rendering, error) {
	a := *snap.Acted
	if a.Result != ResultUnfinished {
		return card.Rendered(card.Update{Status: actedLine(a)}), nil
	}
	op, err := s.d.Operation(ctx, a.OpID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return card.Rendered(card.Update{Status: unfinishedLine}), nil
	case err != nil:
		return card.Rendering{}, err
	case operation.Resumable(op):
		return card.Rendered(resumeUpdate(snap.Key, op)), nil
	case op.Status == operation.Done:
		return card.Rendered(card.Update{Status: actedLine(Acted{Action: ActionRegrab, At: a.At})}), nil
	}
	return card.Rendered(card.Update{Status: unfinishedLine}), nil
}

func resumeUpdate(key Key, op operation.Op) card.Update {
	up := card.Update{Status: "⚠️ " + operation.Line(op)}
	id, ok := card.Encode(card.Action{
		Kind: cardKind, Verb: verbResume,
		Args: []string{appCode(key.App), key.DownloadID, fmt.Sprintf("o%d.%d", op.ID, op.Version)},
	})
	if ok {
		up.Buttons = []card.Button{{Label: "Resume", Style: card.Secondary, ID: id}}
	}
	return up
}
