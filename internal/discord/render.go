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

type builder struct {
	kids      []discordgo.MessageComponent
	publicURL string
}

func (b *builder) text(s string) {
	if s != "" {
		b.kids = append(b.kids, discordgo.TextDisplay{Content: s})
	}
}

func (b *builder) section(s string, accessory discordgo.Button) {
	b.kids = append(b.kids, discordgo.Section{
		Components: []discordgo.MessageComponent{discordgo.TextDisplay{Content: s}},
		Accessory:  accessory,
	})
}

func (b *builder) separator(divider bool, size discordgo.SeparatorSpacingSize) {
	b.kids = append(b.kids, discordgo.Separator{Divider: &divider, Spacing: &size})
}

func render(c card.Card, u map[string]card.Update, ov overlay, publicURL string) []discordgo.MessageComponent {
	b := &builder{publicURL: publicURL}
	cardButtons, tone := c.Buttons, c.Tone
	up, live := u[c.Subject]
	live = live && c.Subject != ""
	if live {
		cardButtons = up.Buttons
		if up.Tone != nil {
			tone = *up.Tone
		}
	}
	link := c.Path != "" && publicURL != ""
	openButton := discordgo.Button{Label: "Open", Style: discordgo.LinkButton, URL: publicURL + c.Path}
	headerLink := link && c.Title != ""
	title := toneEmoji(tone) + c.Title
	if c.Title != "" {
		title = "### " + title
		if headerLink {
			b.section(title, openButton)
		} else {
			b.text(title)
		}
	}
	b.text(c.Body)
	b.text(fieldLines(c.Fields))
	if len(b.kids) > 0 && (len(c.Items) > 0 || len(cardButtons) > 0 || len(ov.Confirm) > 0 || (link && !headerLink)) {
		b.separator(true, discordgo.SeparatorSpacingSizeSmall)
	}
	b.items(c.Items, u)
	if live {
		b.text(statusLine(up.Status))
		b.text(fieldLines(up.Sections))
	}
	b.text(statusLine(ov.Line))
	if len(ov.Confirm) > 0 {
		cardButtons = ov.Confirm
	}
	b.text(statusLine(c.Footer))
	cardComps := buttonComponents(cardButtons, publicURL)
	if link && !headerLink {
		cardComps = append(cardComps, openButton)
	}
	b.kids = append(b.kids, rows(cardComps)...)
	accent := accentColor(tone)
	return []discordgo.MessageComponent{discordgo.Container{AccentColor: &accent, Components: b.kids}}
}

func (b *builder) items(items []card.Item, u map[string]card.Update) {
	for i, it := range items {
		if i > 0 {
			b.separator(false, discordgo.SeparatorSpacingSizeSmall)
		}
		itemText, itemButtons := itemView(it, u)
		comps := buttonComponents(itemButtons, b.publicURL)
		if btn, ok := singleButton(comps); ok && itemText != "" {
			b.section(itemText, btn)
			continue
		}
		b.text(itemText)
		b.kids = append(b.kids, rows(comps)...)
	}
}

func singleButton(comps []discordgo.MessageComponent) (discordgo.Button, bool) {
	if len(comps) != 1 {
		return discordgo.Button{}, false
	}
	btn, ok := comps[0].(discordgo.Button)
	return btn, ok
}

func statusLine(s string) string {
	if s == "" {
		return ""
	}
	return "-# " + s
}

func toneEmoji(t card.Tone) string {
	switch t {
	case card.Success:
		return "✅ "
	case card.Warn:
		return "⚠️ "
	case card.Alert:
		return "🚨 "
	case card.Info:
	}
	return ""
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
		lines = append(lines, "**"+f.Name+"** · "+f.Value)
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
