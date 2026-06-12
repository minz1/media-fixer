package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/minz1/mediafixer/internal/db"
	openai "github.com/sashabaranov/go-openai"
)

const systemPrompt = `You are a media stack diagnostic agent. You help troubleshoot playback problems
in a self-hosted Jellyfin + decypharr (debrid FUSE mount) setup.

Media files live under /mnt/decypharr. Cache is at /var/cache/decypharr. Other data is at /data.

Diagnostic procedure — run in order, stop when you find the root cause:
1. Call jellyfin_playback_info. The response includes MediaSources[].Path — the actual file path
   on disk. Use that path (and only that path) for dd_readability_test. Never construct or guess paths.
2. If MediaSources is empty (Jellyfin can't open the file), call get_disk_info first to confirm
   the /mnt/decypharr mount is present and healthy before drawing conclusions.
3. Call dd_readability_test using the exact path from step 1. EIO errors or very low bytes-read
   confirm a FUSE/debrid link problem.
4. Call get_torrent_state to check decypharr's view of the torrent.
5. Call loki_query for jellyfin and decypharr logs around the report time.

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
	llm   *openai.Client
	model string
	disp  *Dispatcher
	db    *db.DB
	log   *slog.Logger
}

func New(llm *openai.Client, model string, disp *Dispatcher, database *db.DB, log *slog.Logger) *Agent {
	return &Agent{llm: llm, model: model, disp: disp, db: database, log: log}
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
// Pass seed=nil for a fresh run; pass the previous conversation (with control
// reviewer feedback appended) to continue from where the last run left off.
// Returns the full conversation so the control reviewer can inspect it.
func (a *Agent) Run(ctx context.Context, inc *db.Incident, seed []openai.ChatCompletionMessage) (*DiagnosticResult, []openai.ChatCompletionMessage, error) {
	a.log.Info("starting diagnostic", "incident", inc.ID, "title", inc.Title)

	a.disp.IncidentID = inc.ID

	if err := a.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusInvestigating); err != nil {
		return nil, nil, err
	}

	var messages []openai.ChatCompletionMessage
	if len(seed) > 0 {
		messages = seed
	} else {
		messages = []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: a.buildUserMessage(inc)},
		}
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
			return nil, messages, fmt.Errorf("llm round %d: %w", round, err)
		}

		choice := resp.Choices[0]
		msg := choice.Message
		messages = append(messages, msg)

		a.logTurn(inc.ID, round, msg)

		if len(msg.ToolCalls) == 0 {
			break
		}

		for _, call := range msg.ToolCalls {
			fn := call.Function.Name

			if fn == "complete_diagnosis" {
				var result DiagnosticResult
				if err := json.Unmarshal([]byte(call.Function.Arguments), &result); err != nil {
					return nil, messages, fmt.Errorf("parse complete_diagnosis: %w", err)
				}
				if err := a.db.SetIncidentFinding(ctx, inc.ID, result, result); err != nil {
					a.log.Error("set finding", "error", err)
				}

				if !result.RequiresApproval && inc.JellyfinItemID != "" {
					if !a.verifyFix(ctx, inc.JellyfinItemID) {
						a.log.Warn("post-fix verification failed, escalating", "incident", inc.ID)
						result.RequiresApproval = true
						result.EscalateAction = "autonomous fix applied but playback verification failed"
					}
				}

				return &result, messages, nil
			}

			if isAutonomousAction(fn) {
				autonomousActions++
				_, _ = a.db.IncrementActionCount(ctx, inc.ID)

				if autonomousActions > maxAutonomousActions {
					_ = a.db.SetAutonomousLocked(ctx, inc.ID, true)
					_ = a.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
					return &DiagnosticResult{
						RootCause:        "max autonomous actions reached without resolution",
						Confidence:       "low",
						PrimaryAction:    "manual_investigation",
						PrimaryReason:    "agent applied 3 actions without confirming fix",
						RequiresApproval: true,
					}, messages, nil
				}
			}

			resultJSON := a.disp.Dispatch(ctx, fn, call.Function.Arguments)
			a.log.Debug("tool call", "tool", fn, "result", resultJSON)

			messages = append(messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    resultJSON,
				ToolCallID: call.ID,
			})
		}
	}

	return &DiagnosticResult{
		RootCause:        "diagnostic loop exhausted without conclusion",
		Confidence:       "low",
		PrimaryAction:    "manual_investigation",
		PrimaryReason:    "agent did not reach a conclusion within iteration limit",
		RequiresApproval: true,
	}, messages, nil
}

func (a *Agent) verifyFix(ctx context.Context, itemID string) bool {
	info, err := a.disp.Jellyfin.PlaybackInfo(ctx, itemID)
	return err == nil && len(info.MediaSources) > 0
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
	a.log.Info("agent_turn",
		"incident_id", incidentID,
		"round", round,
		"message", json.RawMessage(b),
	)
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
