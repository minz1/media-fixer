package agent_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/db"
)

func TestA9_4_AttemptHistoryLabelsFailedRows(t *testing.T) {
	t.Parallel()
	database := newTestAgentDB(t)
	ctx := context.Background()

	inc := &db.Incident{Status: db.StatusReopened, Source: "discord", What: "cant_play", Title: "The Boys"}
	if err := database.CreateIncident(ctx, inc); err != nil {
		t.Fatal(err)
	}
	for _, row := range []*db.ActionLog{
		{IncidentID: inc.ID, Action: "clear_jellyfin_cache", TriggeredBy: "agent", Status: db.ActionApplied},
		{
			IncidentID: inc.ID, Action: agent.EscalateRemoveAndSearch, TriggeredBy: "owner",
			Status: db.ActionFailed, Error: "arr replace: delete file 11: 500",
		},
	} {
		if err := database.LogAction(ctx, row); err != nil {
			t.Fatal(err)
		}
	}

	got := agent.New(nil, "", nil, database, nil, slog.New(slog.DiscardHandler)).AttemptHistoryForTest(ctx, inc.ID)

	if strings.Contains(got, "every one of these was applied successfully") {
		t.Errorf("attempt history calls a failed action applied:\n%s", got)
	}
	var cacheLine, replaceLine string
	for line := range strings.SplitSeq(got, "\n") {
		switch {
		case strings.Contains(line, "clear_jellyfin_cache"):
			cacheLine = line
		case strings.Contains(line, agent.EscalateRemoveAndSearch):
			replaceLine = line
		}
	}
	if !strings.Contains(cacheLine, "applied") || strings.Contains(cacheLine, "failed") {
		t.Errorf("applied row line = %q, want it labelled applied", cacheLine)
	}
	if !strings.Contains(replaceLine, "failed") || strings.Contains(replaceLine, "(applied") {
		t.Errorf("failed row line = %q, want it labelled failed", replaceLine)
	}
}
