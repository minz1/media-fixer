package discord_test

import (
	"fmt"
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
		case discordgo.Section:
			out = append(out, flatten(v.Components)...)
			out = append(out, v.Accessory)
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
	for _, want := range []string{"Title here", "Body here", "**Root cause** · x", "the footer"} {
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

func TestRender_UpdateToneSetsAccent(t *testing.T) {
	t.Parallel()
	c := card.Card{Subject: "sweep:1", Tone: card.Info, Title: "Pack sweep started"}
	tone := card.Success
	for _, tc := range []struct {
		name string
		up   card.Update
		want int
	}{
		{"no tone keeps the card's", card.Update{Status: "running"}, 0x5865F2},
		{"update tone wins", card.Update{Status: "done", Tone: &tone}, 0x57F287},
	} {
		got := discord.RenderUpdatesForTest(c, map[string]card.Update{"sweep:1": tc.up})
		box, ok := got[0].(discordgo.Container)
		if !ok || box.AccentColor == nil || *box.AccentColor != tc.want {
			t.Errorf("%s: accent = %v, want %#x", tc.name, box.AccentColor, tc.want)
		}
	}
}

func find[T discordgo.MessageComponent](cs []discordgo.MessageComponent) []T {
	var out []T
	for _, c := range flatten(cs) {
		if v, ok := c.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func TestRender_HeaderSectionWithOpenAccessory(t *testing.T) {
	t.Parallel()
	c := card.Card{Title: "T", Body: "B", Path: "/incidents/x"}
	got := discord.RenderForTest(c, "https://a/media")
	secs := find[discordgo.Section](got)
	if len(secs) != 1 {
		t.Fatalf("sections = %d, want 1", len(secs))
	}
	b, ok := secs[0].Accessory.(discordgo.Button)
	if !ok || b.Label != "Open" || b.Style != discordgo.LinkButton || b.URL != "https://a/media/incidents/x" {
		t.Errorf("accessory = %+v", secs[0].Accessory)
	}
	if n := len(buttons(got)); n != 1 {
		t.Errorf("buttons = %d, want 1 (no separate Dashboard)", n)
	}
	if len(find[discordgo.Section](discord.RenderForTest(c, ""))) != 0 {
		t.Error("section rendered without public_url")
	}
	c.Title = ""
	var rowLinks int
	for _, r := range find[discordgo.ActionsRow](discord.RenderForTest(c, "https://a/media")) {
		rowLinks += len(r.Components)
	}
	if rowLinks != 1 {
		t.Errorf("untitled card bottom buttons = %d, want 1", rowLinks)
	}
}

func TestRender_SeparatorsAndSingleButtonItemSection(t *testing.T) {
	t.Parallel()
	one := []card.Button{{Label: "Go", ID: "mf1:sq:go:s:1"}}
	two := append([]card.Button{{Label: "No", ID: "mf1:sq:no:s:1"}}, one...)
	c := card.Card{Title: "T", Items: []card.Item{
		{Text: "one", Buttons: one}, {Text: "two", Buttons: two}, {Text: "three"},
	}}
	got := discord.RenderForTest(c, "")
	seps := find[discordgo.Separator](got)
	if len(seps) != 3 {
		t.Fatalf("separators = %d, want 3", len(seps))
	}
	if seps[0].Divider == nil || !*seps[0].Divider {
		t.Error("header separator should be a divider")
	}
	for _, s := range seps[1:] {
		if s.Divider == nil || *s.Divider || s.Spacing == nil || *s.Spacing != discordgo.SeparatorSpacingSizeSmall {
			t.Errorf("item gap = %+v, want small no-divider", s)
		}
	}
	secs := find[discordgo.Section](got)
	if len(secs) != 1 || secs[0].Accessory.(discordgo.Button).Label != "Go" {
		t.Fatalf("sections = %+v, want one with Go accessory", secs)
	}
	if rows := find[discordgo.ActionsRow](got); len(rows) != 1 || len(rows[0].Components) != 2 {
		t.Errorf("rows = %+v, want one row of the two-button item", rows)
	}
	if n := len(find[discordgo.Separator](discord.RenderForTest(card.Card{Title: "T"}, ""))); n != 0 {
		t.Errorf("header-only card has %d separators", n)
	}
}

func TestRender_ToneEmoji(t *testing.T) {
	t.Parallel()
	for tone, want := range map[card.Tone]string{
		card.Info: "### T", card.Success: "### ✅ T", card.Warn: "### ⚠️ T", card.Alert: "### 🚨 T",
	} {
		if got := texts(discord.RenderForTest(card.Card{Title: "T", Tone: tone}, "")); got != want+"\n" {
			t.Errorf("tone %d: %q, want %q", tone, got, want)
		}
	}
	ok := card.Success
	c := card.Card{Subject: "s:1", Tone: card.Alert, Title: "T"}
	got := texts(discord.RenderUpdatesForTest(c, map[string]card.Update{"s:1": {Tone: &ok}}))
	if !strings.Contains(got, "### ✅ T") {
		t.Errorf("live tone should win: %q", got)
	}
}

func btns(n int) []card.Button {
	out := make([]card.Button, n)
	for i := range out {
		out[i] = card.Button{Label: "b", ID: fmt.Sprintf("mf1:sq:b%d:s:1", i)}
	}
	return out
}

func matrixCard(title, path string, items, nb int, extras bool) card.Card {
	c := card.Card{Title: title, Path: path, Buttons: btns(nb)}
	if extras {
		c.Body, c.Footer = "B", "f"
		c.Fields = []card.Field{{Name: "N", Value: "v"}}
	}
	for range items {
		c.Items = append(c.Items, card.Item{Text: "i", Buttons: btns(nb)})
	}
	return c
}

func matrixCards() []card.Card {
	var out []card.Card
	for combo := range 4 {
		for _, items := range []int{0, 1, 2, 6} {
			for _, nb := range []int{0, 1, 2, 6} {
				for _, extras := range []bool{false, true} {
					path, title := []string{"", "/x"}[combo%2], []string{"", "T"}[combo/2]
					out = append(out, matrixCard(title, path, items, nb, extras))
				}
			}
		}
	}
	return out
}

func TestRender_CountParityMatrix(t *testing.T) {
	t.Parallel()
	for _, url := range []string{"https://a/media", ""} {
		for _, c := range matrixCards() {
			got, want := len(flatten(discord.RenderForTest(c, url))), c.Components()
			if got > want || (url != "" && got != want) {
				t.Errorf("%q %+v: rendered %d, Components() = %d", url, c, got, want)
			}
		}
	}
}

func TestRender_CountParityOverlayAndUpdates(t *testing.T) {
	t.Parallel()
	c := card.Card{Subject: "s:card", Title: "T", Path: "/x", Verbs: []string{"a", "b"}, Buttons: btns(2),
		Items: []card.Item{{Subject: "s:i", Text: "i", Buttons: btns(2)}}}
	live := map[string]card.Update{
		"s:card": {Status: "running", Buttons: btns(2)},
		"s:i":    {Status: "done", Buttons: btns(1)},
	}
	got := len(flatten(discord.RenderOverlayForTest(c, live, "ok", btns(2), "https://a/media")))
	if want := c.Components() + card.Reserve; got > want {
		t.Errorf("rendered %d, want at most %d", got, want)
	}
}
