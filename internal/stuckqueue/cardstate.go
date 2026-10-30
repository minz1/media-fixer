package stuckqueue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
)

const (
	cardKind     = "sq"
	cardPrefix   = cardKind + ":"
	cardArgs     = 3
	subjectParts = 3
	refusedLine  = "Refused: "
	goneLine     = "left the queue"
)

type outcome struct {
	action Action
	at     time.Time
}

func (o outcome) line() string {
	return fmt.Sprintf("✅ %s · <t:%d:t>", actionDone(o.action), o.at.Unix())
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
	return func(_ context.Context, subject string) (card.Update, bool, error) {
		parts := strings.SplitN(subject, ":", subjectParts)
		if len(parts) != subjectParts || parts[0] != cardKind {
			return card.Update{}, false, nil
		}
		app, ok := appOf(parts[1])
		if !ok {
			return card.Update{}, false, nil
		}
		s.smu.Lock()
		defer s.smu.Unlock()
		if s.running {
			return card.Update{}, false, nil
		}
		if o, acted := s.outcomes[subject]; acted {
			return card.Update{Status: o.line()}, true, nil
		}
		if !s.hasLast {
			return card.Update{}, false, nil
		}
		i := slices.IndexFunc(s.last.Apps, func(ar AppResult) bool { return ar.App == app })
		if i < 0 || !s.last.Apps[i].Configured || s.last.Apps[i].Unreachable {
			return card.Update{}, false, nil
		}
		ar := s.last.Apps[i]
		match := func(it Item) bool { return strings.EqualFold(it.DownloadID, parts[2]) }
		if j := slices.IndexFunc(ar.Items, match); j >= 0 {
			return card.Update{Buttons: itemButtons(ar.Items[j].Download, ar.Items[j].Actions)}, true, nil
		}
		if j := slices.IndexFunc(ar.Waiting, match); j >= 0 {
			w := ar.Waiting[j]
			return card.Update{
				Status:  fmt.Sprintf("⏳ Inside the wait until <t:%d:t>; only Remove until then", w.Due.Unix()),
				Buttons: itemButtons(w.Download, w.Actions),
			}, true, nil
		}
		return card.Update{Status: goneLine}, true, nil
	}
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
		notice, err := s.Act(ctx, app, a.Args[1], Action(a.Verb), a.Args[2])
		if errors.Is(err, ErrRefused) {
			return card.Result{Line: refusedLine + strings.TrimPrefix(err.Error(), ErrRefused.Error()+": ")}, nil
		}
		if err != nil {
			return card.Result{}, err
		}
		return card.Result{Line: notice}, nil
	}
}

func (s *Scheduler) recordOutcome(d Download, a Action) {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.outcomes == nil {
		s.outcomes = map[string]outcome{}
	}
	s.outcomes[subjectOf(d)] = outcome{action: a, at: s.r.Now()}
}

func (s *Scheduler) refresh(subject string) {
	if s.Refresh != nil {
		s.Refresh(subject)
	}
}
