package sweep

import (
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/db"
)

const maintenancePath = "/maintenance"

func summaryCard(sr db.SweepRun) card.Card {
	kind := "Pack sweep"
	if sr.Dry {
		kind = "Pack sweep dry run"
	}
	status := sr.Status
	if sr.Reason != "" {
		status += " (" + sr.Reason + ")"
	}
	tone := card.Warn
	if sr.Status == statusDone {
		tone = card.Success
	}
	c := sr.Counts
	body := fmt.Sprintf(
		"grabs %d/%d, lookups %d/%d, searched %d/%d, imported %d, failed %d, refused %d, still downloading %d",
		c.Grabs, c.MaxGrabs, c.Lookups, c.MaxLookups, c.Searched, c.MaxSearches,
		c.Imported, c.Failed, c.Refused, c.Downloading,
	)
	if sr.FinishedAt != nil {
		body += ", took " + sr.FinishedAt.Sub(sr.StartedAt).Round(time.Second).String()
	}
	return card.Card{
		Tone:  tone,
		Title: kind + " " + status + ":",
		Body:  body + ". /media/maintenance",
		Path:  maintenancePath,
	}
}
