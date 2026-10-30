package discord_test

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/discord"
)

func flatten(cs []discordgo.MessageComponent) []discordgo.MessageComponent {
	var out []discordgo.MessageComponent
	for _, c := range cs {
		out = append(out, c)
		switch v := c.(type) {
		case discordgo.Container:
			out = append(out, flatten(v.Components)...)
		case discordgo.ActionsRow:
			out = append(out, flatten(v.Components)...)
		}
	}
	return out
}

func texts(cs []discordgo.MessageComponent) string {
	var b strings.Builder
	for _, c := range flatten(cs) {
		if t, ok := c.(discordgo.TextDisplay); ok {
			b.WriteString(t.Content + "\n")
		}
	}
	return b.String()
}

func buttons(cs []discordgo.MessageComponent) []discordgo.Button {
	var out []discordgo.Button
	for _, c := range flatten(cs) {
		if b, ok := c.(discordgo.Button); ok {
			out = append(out, b)
		}
	}
	return out
}

func TestRender_ContainerAccentAndText(t *testing.T) {
	t.Parallel()
	c := card.Card{
		Tone:   card.Warn,
		Title:  "Title here",
		Body:   "Body here",
		Fields: []card.Field{{Name: "Root cause", Value: "x"}},
		Footer: "the footer",
	}
	got := discord.RenderForTest(c, "")
	if len(got) != 1 {
		t.Fatalf("top-level components = %d, want 1", len(got))
	}
	box, ok := got[0].(discordgo.Container)
	if !ok {
		t.Fatalf("top-level = %T, want Container", got[0])
	}
	if box.AccentColor == nil || *box.AccentColor != 0xFEE75C {
		t.Errorf("accent = %v, want 0xFEE75C", box.AccentColor)
	}
	all := texts(got)
	for _, want := range []string{"Title here", "Body here", "**Root cause:** x", "the footer"} {
		if !strings.Contains(all, want) {
			t.Errorf("text %q missing from %q", want, all)
		}
	}
}

func TestRender_LinkOnlyWithPublicURL(t *testing.T) {
	t.Parallel()
	c := card.Card{Title: "T", Path: "/incidents/x"}
	if b := buttons(discord.RenderForTest(c, "")); len(b) != 0 {
		t.Errorf("buttons without public_url = %d, want 0", len(b))
	}
	b := buttons(discord.RenderForTest(c, "https://a/media"))
	if len(b) != 1 || b[0].Style != discordgo.LinkButton || b[0].URL != "https://a/media/incidents/x" {
		t.Errorf("link buttons = %+v", b)
	}
}

func TestRender_RowsOfFive(t *testing.T) {
	t.Parallel()
	c := card.Card{Title: "T"}
	for range 7 {
		c.Buttons = append(c.Buttons, card.Button{Label: "b", ID: "mf1:inc:x:y"})
	}
	var rows []int
	for _, comp := range flatten(discord.RenderForTest(c, "")) {
		if r, ok := comp.(discordgo.ActionsRow); ok {
			rows = append(rows, len(r.Components))
		}
	}
	if len(rows) != 2 || rows[0] != 5 || rows[1] != 2 {
		t.Errorf("rows = %v, want [5 2]", rows)
	}
}

func TestRender_CountMatchesComponents(t *testing.T) {
	t.Parallel()
	three := []card.Button{
		{Label: "a", ID: "mf1:sq:import:s:1"},
		{Label: "b", ID: "mf1:sq:regrab:s:1"},
		{Label: "c", Style: card.Danger, ID: "mf1:sq:remove:s:1"},
	}
	first := card.Card{
		Title:   "T",
		Body:    "B",
		Items:   []card.Item{{Text: "one", Buttons: three}, {Text: "two", Buttons: three}},
		Buttons: []card.Button{{Label: "Resolve", ID: "mf1:inc:resolve:x"}},
		Path:    "/incidents/x",
	}
	stuck := card.Card{Title: "📦 Stuck", Footer: "…and 2 more", Path: "/maintenance"}
	for range 6 {
		stuck.Items = append(stuck.Items, card.Item{Text: "item", Buttons: three})
	}
	for _, c := range []card.Card{first, stuck} {
		if got, want := len(flatten(discord.RenderForTest(c, "https://a/media"))), c.Components(); got != want {
			t.Errorf("%q: rendered %d components, Components() = %d", c.Title, got, want)
		}
	}
}
