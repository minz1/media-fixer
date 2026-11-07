package stuckqueue

import (
	"fmt"
	"unicode/utf8"

	"github.com/minz1/mediafixer/internal/card"
)

const maintenancePath = "/maintenance"

func stuckCard(items []Item, capHit bool) (card.Card, int) {
	capLine := fmt.Sprintf("Stopped after clearing %d this run.", maxAutoClears)
	c := card.Card{Tone: card.Warn, Path: maintenancePath}
	if len(items) == 0 {
		c.Title = "Stuck-queue sweep: " + capLine
		return c, 0
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
			if i == 0 {
				return fitFirst(c, len(items)-1, tail, capLine, capHit), 1
			}
			c.Items = c.Items[:i]
			return c, i
		}
	}
	c.Footer = endFooter(capLine, capHit)
	return c, len(items)
}

func endFooter(capLine string, capHit bool) string {
	if capHit {
		return capLine
	}
	return ""
}

func fitFirst(c card.Card, rest int, tail, capLine string, capHit bool) card.Card {
	c.Footer = endFooter(capLine, capHit)
	if rest > 0 {
		c.Footer = fmt.Sprintf("…and %d more", rest) + tail
	}
	text := c.Items[0].Text
	keep := len(text) - (len(c.Plain()) - card.MaxPlain) - len("…")
	keep = max(0, min(keep, len(text)))
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	c.Items[0].Text = text[:keep] + "…"
	return c
}
