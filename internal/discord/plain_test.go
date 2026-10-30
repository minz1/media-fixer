package discord_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/discord"
)

func TestPlainText_CutsOnRuneBoundary(t *testing.T) {
	t.Parallel()
	got := discord.PlainTextForTest(card.Card{Body: "a" + strings.Repeat("é", card.MaxPlain)})
	if len(got) > card.MaxPlain || !utf8.ValidString(got) || len(got) < card.MaxPlain-1 {
		t.Errorf("len %d, valid %v", len(got), utf8.ValidString(got))
	}
	if short := discord.PlainTextForTest(card.Card{Title: "T", Body: "B"}); short != "T\nB" {
		t.Errorf("short = %q", short)
	}
}
