package discord

import (
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
)

func NewForTest(log *slog.Logger, connect func() error, retryFirst, retryCap time.Duration) (*Bot, error) {
	b, err := New("x", "", "", log)
	if err != nil {
		return nil, err
	}
	b.connect, b.retryFirst, b.retryCap = connect, retryFirst, retryCap
	return b, nil
}

func ReportCommandForTest() *discordgo.ApplicationCommand { return reportCommand() }
