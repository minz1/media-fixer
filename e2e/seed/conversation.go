package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
)

type step struct {
	name, args, result string
}

func call(id int, s step) openai.ToolCall {
	return openai.ToolCall{
		ID: fmt.Sprintf("call_%d", id), Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: s.name, Arguments: s.args},
	}
}

func lines(format string, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(format, i+1)
	}
	return strings.Join(out, "\n")
}

const (
	historyLines = 48
	seasonFiles  = 12
)

type script struct {
	seed   []openai.ChatCompletionMessage
	rounds [][]step
	final  string
}

func conversation(title string) script {
	seed := []openai.ChatCompletionMessage{
		{
			Role: openai.ChatMessageRoleSystem,
			Content: "You are media-fixer, a diagnostic agent for Sonarr/Radarr/Plex.\n" +
				"Investigate the report with the tools provided. Never guess: read state first, then act.\n" +
				"Finish by calling complete_diagnosis with a root cause and a recommended action.",
		},
		{
			Role:    openai.ChatMessageRoleUser,
			Content: "Incident: " + title + "\nReported by viewer via Discord.\nDetails: Reported from the e2e seed.",
		},
	}
	rounds := [][]step{
		{
			{
				"get_sonarr_queue",
				`{"series_id": 4}`,
				"{\n  \"totalRecords\": 1,\n  \"records\": [\n    {\n      \"title\": \"Samurai.Champloo.S01E05.1080p.BluRay\",\n" +
					"      \"status\": \"completed\",\n      \"trackedDownloadState\": \"importBlocked\",\n" +
					"      \"statusMessages\": [\"No audio tracks detected in sample\"]\n    }\n  ]\n}",
			},
		},
		{
			{
				"list_directory",
				`{"path": "/dl/ep5"}`,
				"Samurai.Champloo.S01E05.1080p.BluRay.mkv  1.4 GiB\nsample.mkv  12 MiB\nSamurai.Champloo.S01E05.nfo  2 KiB",
			},
			{
				"check_file_readable",
				`{"path": "/dl/ep5.mkv"}`,
				"readable: true\nsize: 1503238553\nmode: 0644\nowner: media:media",
			},
		},
		{
			{
				"get_sonarr_history",
				`{"series_id": 4}`,
				lines(
					"2026-10-%02d 21:14 downloadFolderImported | importBlocked | sample audio check failed",
					historyLines,
				),
			},
		},
	}
	final := "Root cause: Sonarr blocked the import because the sample file has no audio track; the main file is intact and readable.\n" +
		"Recommended action: retry_import."
	return script{seed, rounds, final}
}

func seedConversation(ctx context.Context, jr *journal.Journal, id, title string) error {
	sc := conversation(title)
	if err := seedRounds(ctx, jr, id, sc.seed, sc.rounds); err != nil {
		return err
	}
	last := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: sc.final}
	return jr.LLMRound(ctx, id, len(sc.rounds)+1, last, nil)
}

func seedRounds(
	ctx context.Context, jr *journal.Journal, id string, seed []openai.ChatCompletionMessage, rounds [][]step,
) error {
	if err := jr.RunStarted(ctx, id, seed); err != nil {
		return err
	}
	n := 0
	for r, steps := range rounds {
		if err := seedRound(ctx, jr, id, r+1, steps, &n); err != nil {
			return err
		}
	}
	return nil
}

func seedRound(ctx context.Context, jr *journal.Journal, id string, round int, steps []step, n *int) error {
	asst := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant}
	var tools []openai.ChatCompletionMessage
	for _, s := range steps {
		*n++
		asst.ToolCalls = append(asst.ToolCalls, call(*n, s))
		tools = append(tools, openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleTool, ToolCallID: fmt.Sprintf("call_%d", *n), Name: s.name, Content: s.result,
		})
	}
	return jr.LLMRound(ctx, id, round, asst, tools)
}

func seedInvestigating(ctx context.Context, d *db.DB) (map[string]string, map[string]string, error) {
	title := "Frieren S01E07 audio out of sync"
	inc := &db.Incident{
		Status: db.StatusOpen, Source: "discord", ReportedBy: "viewer", What: "playback",
		Title: title, Details: "Reported from the e2e seed.",
	}
	if err := d.CreateIncident(ctx, inc); err != nil {
		return nil, nil, err
	}
	if err := d.UpdateIncidentStatus(ctx, inc.ID, db.StatusInvestigating); err != nil {
		return nil, nil, err
	}
	rounds := [][]step{
		{{"get_sonarr_queue", `{"series_id": 5}`, "{\n  \"totalRecords\": 0,\n  \"records\": []\n}"}},
		{{"list_directory", `{"path": "/tv/S01"}`, lines("Frieren.S01E%02d.mkv  1.2 GiB", seasonFiles)}},
	}
	if err := seedRounds(ctx, journal.New(d), inc.ID, conversation(title).seed, rounds); err != nil {
		return nil, nil, err
	}
	return map[string]string{"investigating": inc.ID}, map[string]string{title: string(db.StatusInvestigating)}, nil
}
