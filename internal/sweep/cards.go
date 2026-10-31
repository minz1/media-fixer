package sweep

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
)

const (
	maintenancePath = "/maintenance"
	cardPrefix      = "sweep:"
	statusRunning   = "running"
)

func runKind(dry bool) string {
	if dry {
		return "Pack sweep dry run"
	}
	return "Pack sweep"
}

func statusText(sr db.SweepRun) string {
	status := sr.Status
	if sr.Reason != "" {
		status += " (" + sr.Reason + ")"
	}
	return status
}

func runTone(sr db.SweepRun) card.Tone {
	if sr.Status == statusDone {
		return card.Success
	}
	return card.Warn
}

func countsText(c db.SweepCounts, dry bool) string {
	left := ""
	if !dry {
		left = " (limits: left today)"
	}
	return fmt.Sprintf(
		"grabs %d/%d, lookups %d/%d, searched %d/%d%s, imported %d, failed %d, refused %d, still downloading %d",
		c.Grabs, c.MaxGrabs, c.Lookups, c.MaxLookups, c.Searched, c.MaxSearches, left,
		c.Imported, c.Failed, c.Refused, c.Downloading,
	)
}

func took(sr db.SweepRun) string {
	if sr.FinishedAt == nil {
		return ""
	}
	return ", took " + sr.FinishedAt.Sub(sr.StartedAt).Round(time.Second).String()
}

func startCard(id int64, trigger string, dry bool, s Settings) card.Card {
	left := ""
	if !dry {
		left = " left today"
	}
	return card.Card{
		Subject: card.SweepSubject(id),
		Tone:    card.Info,
		Title:   runKind(dry) + " started",
		Fields: []card.Field{
			{Name: "Trigger", Value: trigger},
			{Name: "Budgets", Value: fmt.Sprintf(
				"%d grabs, %d lookups, %d searches%s", s.MaxGrabs, s.MaxLookups, s.MaxSearches, left,
			)},
		},
		Path: maintenancePath,
	}
}

func summaryCard(sr db.SweepRun) card.Card {
	return card.Card{
		Tone:  runTone(sr),
		Title: runKind(sr.Dry) + " " + statusText(sr) + ":",
		Body:  countsText(sr.Counts, sr.Dry) + took(sr) + ". /media/maintenance",
		Path:  maintenancePath,
	}
}

func runID(subject string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(subject, cardPrefix), 10, 64)
	return id, err == nil
}

func (r *Runner) CardRenderer() card.Renderer {
	return func(ctx context.Context, subject string) (card.Update, bool, error) {
		id, ok := runID(subject)
		if !ok {
			return card.Update{}, false, nil
		}
		if p, live := r.Progress(); live && p.RunID == id {
			status := "Running: " + p.Phase
			if p.Current != "" {
				status += " · " + p.Current
			}
			counts := countsText(p.Counts, p.Dry)
			return card.Update{
				Status: status, Sections: []card.Field{{Name: "Counts", Value: counts}}, Live: true,
			}, true, nil
		}
		sr, err := r.DB.GetSweepRun(ctx, id)
		if errors.Is(err, db.ErrNotFound) || (err == nil && sr.Status == statusRunning) {
			return card.Update{}, false, nil
		}
		if err != nil {
			return card.Update{}, false, err
		}
		tone := runTone(sr)
		return card.Update{
			Status:   statusText(sr) + took(sr),
			Sections: []card.Field{{Name: "Counts", Value: countsText(sr.Counts, sr.Dry)}},
			Tone:     &tone,
		}, true, nil
	}
}
