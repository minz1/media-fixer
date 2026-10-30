package discord_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/minz1/mediafixer/internal/card"
	"github.com/minz1/mediafixer/internal/discord"
)

type fakeSender struct {
	mu       sync.Mutex
	complex  []*discordgo.MessageSend
	plain    []string
	complErr error
}

func (f *fakeSender) UserChannelCreate(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: "dm"}, nil
}

func (f *fakeSender) ChannelMessageSendComplex(
	_ string, m *discordgo.MessageSend, _ ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.complex = append(f.complex, m)
	if f.complErr != nil {
		return nil, f.complErr
	}
	return &discordgo.Message{ID: "m1"}, nil
}

func (f *fakeSender) ChannelMessageSend(_, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain = append(f.plain, content)
	return &discordgo.Message{ID: "m2"}, nil
}

func (f *fakeSender) ChannelMessageEditComplex(
	*discordgo.MessageEdit, ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	return &discordgo.Message{}, nil
}

func (f *fakeSender) InteractionRespond(
	*discordgo.Interaction, *discordgo.InteractionResponse, ...discordgo.RequestOption,
) error {
	return nil
}

func (f *fakeSender) InteractionResponseEdit(
	*discordgo.Interaction, *discordgo.WebhookEdit, ...discordgo.RequestOption,
) (*discordgo.Message, error) {
	return &discordgo.Message{}, nil
}

func sendBot(api *fakeSender) (*discord.Bot, *bytes.Buffer) {
	var logs bytes.Buffer
	return discord.NewWithSenderForTest(slog.New(slog.NewTextHandler(&logs, nil)), api, "owner", ""), &logs
}

func TestSend_SetsV2Flag(t *testing.T) {
	t.Parallel()
	api := &fakeSender{}
	b, logs := sendBot(api)
	if err := b.NotifyOwner(t.Context(), card.Card{Title: "T"}); err != nil {
		t.Fatal(err)
	}
	if len(api.complex) != 1 || len(api.plain) != 0 {
		t.Fatalf("complex %d, plain %d", len(api.complex), len(api.plain))
	}
	m := api.complex[0]
	if m.Flags&discordgo.MessageFlagsIsComponentsV2 == 0 || m.Content != "" {
		t.Errorf("flags %v content %q", m.Flags, m.Content)
	}
	l := logs.String()
	if strings.Count(l, "discord card sent") != 1 || !strings.Contains(l, "plain=false") {
		t.Errorf("logs = %q", l)
	}
}

func TestSend_4xxFallsBackToPlain(t *testing.T) {
	t.Parallel()
	api := &fakeSender{complErr: &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusBadRequest}}}
	b, logs := sendBot(api)
	_, plain, err := discord.SendForTest(t.Context(), b, "u", card.Card{Title: "T", Body: "B"})
	if err != nil || !plain {
		t.Fatalf("plain %v err %v", plain, err)
	}
	if len(api.plain) != 1 || api.plain[0] != "T\nB" {
		t.Errorf("plain sends = %q", api.plain)
	}
	if l := logs.String(); !strings.Contains(l, "plain=true") || !strings.Contains(l, "status=400") {
		t.Errorf("logs = %q", l)
	}
}

func TestSend_TimeoutNoResend(t *testing.T) {
	t.Parallel()
	api := &fakeSender{complErr: context.DeadlineExceeded}
	b, logs := sendBot(api)
	err := b.NotifyUser(t.Context(), "u", card.Card{Title: "T"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	if len(api.plain) != 0 || strings.Contains(logs.String(), "discord card sent") {
		t.Errorf("plain %q logs %q", api.plain, logs.String())
	}
}
