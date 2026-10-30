package discord

import (
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
)

const (
	accentInfo    = 0x5865F2
	accentSuccess = 0x57F287
	accentWarn    = 0xFEE75C
	accentAlert   = 0xED4245
)

type overlay struct {
	Line       string
	Confirm    []card.Button
	At         time.Time
	UpdateHash string
}

func render(c card.Card, u map[string]card.Update, ov overlay, publicURL string) []discordgo.MessageComponent {
	var kids []discordgo.MessageComponent
	text := func(s string) {
		if s != "" {
			kids = append(kids, discordgo.TextDisplay{Content: s})
		}
	}
	if c.Title != "" {
		text("### " + c.Title)
	}
	text(c.Body)
	text(fieldLines(c.Fields))
	for _, it := range c.Items {
		itemText, itemButtons := itemView(it, u)
		text(itemText)
		kids = append(kids, rows(buttonComponents(itemButtons, publicURL))...)
	}
	cardButtons, tone := c.Buttons, c.Tone
	if up, ok := u[c.Subject]; ok && c.Subject != "" {
		cardButtons = up.Buttons
		if up.Tone != nil {
			tone = *up.Tone
		}
		if up.Status != "" {
			text("-# " + up.Status)
		}
		text(fieldLines(up.Sections))
	}
	if ov.Line != "" {
		text("-# " + ov.Line)
	}
	if len(ov.Confirm) > 0 {
		cardButtons = ov.Confirm
	}
	if c.Footer != "" {
		text("-# " + c.Footer)
	}
	cardComps := buttonComponents(cardButtons, publicURL)
	if c.Path != "" && publicURL != "" {
		cardComps = append(
			cardComps,
			discordgo.Button{Label: "Dashboard", Style: discordgo.LinkButton, URL: publicURL + c.Path},
		)
	}
	kids = append(kids, rows(cardComps)...)
	accent := accentColor(tone)
	return []discordgo.MessageComponent{discordgo.Container{AccentColor: &accent, Components: kids}}
}

func itemView(it card.Item, u map[string]card.Update) (string, []card.Button) {
	up, ok := u[it.Subject]
	if !ok || it.Subject == "" {
		return it.Text, it.Buttons
	}
	if up.Status == "" {
		return it.Text, up.Buttons
	}
	return it.Text + "\n-# " + up.Status, up.Buttons
}

func fieldLines(fs []card.Field) string {
	lines := make([]string, 0, len(fs))
	for _, f := range fs {
		lines = append(lines, "**"+f.Name+":** "+f.Value)
	}
	return strings.Join(lines, "\n")
}

func buttonComponents(bs []card.Button, publicURL string) []discordgo.MessageComponent {
	out := make([]discordgo.MessageComponent, 0, len(bs))
	for _, b := range bs {
		if b.Path != "" {
			if publicURL != "" {
				out = append(
					out,
					discordgo.Button{Label: b.Label, Style: discordgo.LinkButton, URL: publicURL + b.Path},
				)
			}
			continue
		}
		out = append(out, discordgo.Button{Label: b.Label, Style: buttonStyle(b.Style), CustomID: b.ID})
	}
	return out
}

func rows(bs []discordgo.MessageComponent) []discordgo.MessageComponent {
	var out []discordgo.MessageComponent
	for chunk := range slices.Chunk(bs, card.RowSize) {
		out = append(out, discordgo.ActionsRow{Components: chunk})
	}
	return out
}

func buttonStyle(s card.Style) discordgo.ButtonStyle {
	switch s {
	case card.Primary:
		return discordgo.PrimaryButton
	case card.Secondary:
		return discordgo.SecondaryButton
	case card.Danger:
		return discordgo.DangerButton
	case card.Link:
		return discordgo.LinkButton
	}
	return discordgo.SecondaryButton
}

func accentColor(t card.Tone) int {
	switch t {
	case card.Info:
		return accentInfo
	case card.Success:
		return accentSuccess
	case card.Warn:
		return accentWarn
	case card.Alert:
		return accentAlert
	}
	return accentInfo
}
