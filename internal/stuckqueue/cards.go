package stuckqueue

import (
	"fmt"

	"github.com/minz1/mediafixer/internal/card"
)

const maintenancePath = "/maintenance"

func stuckCard(items []Item, capHit bool) card.Card {
	capLine := fmt.Sprintf("Stopped after clearing %d this run.", maxAutoClears)
	c := card.Card{Tone: card.Warn, Path: maintenancePath}
	if len(items) == 0 {
		c.Title = "📦 Stuck-queue sweep: " + capLine
		return c
	}
	c.Title = dmHeader
	tail := ""
	if capHit {
		tail = "\n" + capLine
	}
	for i, it := range items {
		c.Items = append(c.Items, card.Item{
			Subject: subjectOf(it.Download),
			Text:    it.App.Name() + " — " + it.Title + " — " + reason(it),
			Buttons: itemButtons(it.Download, it.Actions),
		})
		c.Footer = fmt.Sprintf("…and %d more", len(items)-i) + tail
		if len(c.Plain()) > card.MaxPlain || c.Components() > card.MaxBuilt {
			c.Items = c.Items[:i]
			return c
		}
	}
	c.Footer = capLine
	if !capHit {
		c.Footer = ""
	}
	return c
}
