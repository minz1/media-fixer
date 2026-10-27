package discord_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/discord"
)

func TestReportCommand_OffersBuffering(t *testing.T) {
	t.Parallel()
	cmd := discord.ReportCommandForTest()
	for _, opt := range cmd.Options {
		if opt.Name != "what" {
			continue
		}
		for _, c := range opt.Choices {
			if c.Value == "buffering" {
				if c.Name != "It plays but buffers / low quality" {
					t.Errorf("buffering choice name = %q", c.Name)
				}
				return
			}
		}
		t.Fatalf("what choices = %+v, want a buffering choice", opt.Choices)
	}
	t.Fatal("report command has no what option")
}
