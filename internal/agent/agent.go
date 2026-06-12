package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	openai "github.com/sashabaranov/go-openai"
	"github.com/rs/zerolog"
)

const systemPrompt = `You are a media stack diagnostic agent. You help troubleshoot playback problems
in a self-hosted Jellyfin + decypharr (debrid FUSE mount) setup.

Diagnostic procedure — run in order, stop when you find the root cause:
1. Call jellyfin_playback_info. If MediaSources is empty, Jellyfin can't open the file.
2. Call dd_readability_test on the file path from step 1 (or a likely path if not found).
   EIO errors or very low bytes-read confirm a FUSE/debrid link problem.
3. Call get_torrent_state to check decypharr's view of the torrent.
4. Call loki_query for jellyfin and decypharr logs around the report time.

After diagnosis, call complete_diagnosis with your conclusion. Be concise and specific.

Action priority (least destructive first):
1. refresh_decypharr_links  — for EIO / stale CDN URLs
2. decypharr_repair_sweep   — general broken-entry check
3. restart_decypharr        — if decypharr appears stuck
4. sonarr_rescan / radarr_rescan — if Jellyfin sees no sources but file might be present
5. clear_jellyfin_cache     — if metadata is stale

You may call autonomous actions directly. Approval-required actions
(delete torrent, blocklist + search) must only appear in complete_diagnosis.escalate_action.

Max 3 autonomous actions before you must complete_diagnosis regardless.`

// Agent orchestrates the LLM diagnostic loop for one incident.
type Agent struct {
	llm    *openai.Client
	model  string
	disp   *Dispatcher
	db     *db.DB
	log    zerolog.Logger
}

func New(llm *openai.Client, model string, disp *Dispatcher, database *db.DB, log zerolog.Logger) *Agent {
	return &Agent{
		llm:   llm,
		model: model,
		disp:  disp,
		db:    database,
		log:   log,
	}
}

// DiagnosticResult is the structured output from complete_diagnosis.
type DiagnosticResult struct {
	RootCause        string `json:"root_cause"`
	Confidence       string `json:"confidence"`
	PrimaryAction    string `json:"primary_action"`
	PrimaryReason    string `json:"primary_reason"`
	FallbackAction   string `json:"fallback_action,omitempty"`
	EscalateAction   string `json:"escalate_action,omitempty"`
	RequiresApproval bool   `json:"requires_approval"`
}

// Run executes the diagnostic loop for the given incident.
// It updates the incident status and finding as it goes.
func (a *Agent) Run(ctx context.Context, inc *db.Incident) (*DiagnosticResult, error) {
	a.log.Info().Str("incident", inc.ID).Str("title", inc.Title).Msg("starting diagnostic")

	if err := a.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusInvestigating); err != nil {
		return nil, err
	}

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: a.buildUserMessage(inc)},
	}

	tools := toolDefs()
	autonomousActions := 0
	const maxAutonomousActions = 3
	const maxRounds = 20

	for round := 0; round < maxRounds; round++ {
		resp, err := a.llm.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:    a.model,
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return nil, fmt.Errorf("llm round %d: %w", round, err)
		}

		choice := resp.Choices[0]
		msg := choice.Message
		messages = append(messages, msg)

		// Log raw LLM turn for audit.
		a.logTurn(inc.ID, round, msg)

		// No tool calls → LLM finished with a text response.
		if len(msg.ToolCalls) == 0 {
			break
		}

		for _, call := range msg.ToolCalls {
			fn := call.Function.Name

			// complete_diagnosis ends the loop.
			if fn == "complete_diagnosis" {
				var result DiagnosticResult
				if err := json.Unmarshal([]byte(call.Function.Arguments), &result); err != nil {
					return nil, fmt.Errorf("parse complete_diagnosis: %w", err)
				}
				if err := a.db.SetIncidentFinding(ctx, inc.ID, result, result); err != nil {
					a.log.Error().Err(err).Msg("set finding")
				}
				return &result, nil
			}

			// Count autonomous action calls.
			if isAutonomousAction(fn) {
				autonomousActions++
				if autonomousActions > maxAutonomousActions {
					// Lock autonomous actions and surface for escalation.
					_ = a.db.SetAutonomousLocked(ctx, inc.ID, true)
					_ = a.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
					return &DiagnosticResult{
						RootCause:        "max autonomous actions reached without resolution",
						Confidence:       "low",
						PrimaryAction:    "manual_investigation",
						PrimaryReason:    "agent applied 3 actions without confirming fix",
						RequiresApproval: true,
					}, nil
				}
			}

			resultJSON := a.disp.Dispatch(ctx, fn, call.Function.Arguments)
			a.log.Debug().Str("tool", fn).RawJSON("result", []byte(resultJSON)).Msg("tool call")

			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    resultJSON,
				ToolCallID: call.ID,
			})
		}
	}

	// Fell through without complete_diagnosis — synthesize a result.
	return &DiagnosticResult{
		RootCause:        "diagnostic loop exhausted without conclusion",
		Confidence:       "low",
		PrimaryAction:    "manual_investigation",
		PrimaryReason:    "agent did not reach a conclusion within iteration limit",
		RequiresApproval: true,
	}, nil
}

func (a *Agent) buildUserMessage(inc *db.Incident) string {
	return fmt.Sprintf(`New incident reported.
Title: %s
Problem type: %s
Source: %s
Reported by: %s
Details: %s
Jellyfin item ID: %s
Report time: %s

Please diagnose the root cause and apply the least-destructive fix(es) autonomously.
Call complete_diagnosis when done.`,
		inc.Title,
		inc.What,
		inc.Source,
		inc.ReportedBy,
		inc.Details,
		inc.JellyfinItemID,
		inc.CreatedAt.Format(time.RFC3339),
	)
}

func (a *Agent) logTurn(incidentID string, round int, msg openai.ChatCompletionMessage) {
	b, _ := json.Marshal(msg)
	a.log.Info().
		Str("incident_id", incidentID).
		Int("round", round).
		RawJSON("message", b).
		Msg("agent_turn")
}

func isAutonomousAction(toolName string) bool {
	switch toolName {
	case "refresh_decypharr_links",
		"decypharr_repair_sweep",
		"restart_decypharr",
		"restart_jellyfin",
		"sonarr_rescan",
		"radarr_rescan",
		"clear_jellyfin_cache":
		return true
	}
	return false
}
