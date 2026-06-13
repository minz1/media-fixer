package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/db"
)

const systemPrompt = `You are a media stack diagnostic agent. You help troubleshoot problems
in a self-hosted Jellyfin + decypharr (debrid FUSE mount) setup.

IMPORTANT: You are fully autonomous. There is no user to interact with. Never ask questions,
never request clarification. If something is ambiguous, make a reasonable assumption and proceed.

Media files live under /mnt/decypharr. Cache is at /var/cache/decypharr. Other data is at /data.

--- Playback problems (what=cant_play, missing_media) ---

Diagnostic procedure — run in order, stop when you find the root cause:
1. You MUST call jellyfin_playback_info every time. If the incident has no Jellyfin item ID,
   call jellyfin_search first to find it — never skip this step. The response includes
   MediaSources[].Path — the actual file path on disk. Use that exact path for dd_readability_test.
   Never construct or guess a path.
2. If MediaSources is empty (Jellyfin can't open the file):
   a. Call get_disk_info to confirm the /mnt/decypharr mount is present.
   b. Call get_torrent_state to get the torrent folder name.
   c. Call list_directory on /mnt/decypharr/<folder> to find the actual video file(s).
   d. Use the specific file path (not the folder) for dd_readability_test.
3. Call dd_readability_test on the specific file path from step 1 or 2d. Never pass a directory —
   dd on a directory is meaningless. EIO errors or very low bytes-read confirm a FUSE/debrid link problem.
4. Call get_torrent_state to check decypharr's view of the torrent.
5. Call loki_query for jellyfin and decypharr logs around the report time.

--- Infrastructure/connectivity problems (what=other, login_failed, or title is not a media title) ---

The report describes a service or connectivity issue rather than a specific media item. Skip steps 1-3.
1. Call loki_query for jellyfin and decypharr errors in the last 30 minutes.
2. Call get_disk_info to check mount health.
3. If logs show Jellyfin errors or the mount is missing, apply the least-destructive fix.

After diagnosis, call complete_diagnosis with your conclusion. Be concise and specific.
Once you have applied an autonomous action, call complete_diagnosis immediately — do not
keep querying logs or torrent state hoping to observe the effect. If verification is
needed, set requires_approval=false and include it in primary_reason.

Action priority (least destructive first):
1. refresh_decypharr_links  — for EIO / stale CDN URLs
2. decypharr_repair_sweep   — general broken-entry check
3. restart_decypharr        — if decypharr appears stuck
4. sonarr_rescan / radarr_rescan — if Jellyfin sees no sources but file might be present
5. clear_jellyfin_cache     — if metadata is stale

You may call autonomous actions directly. Approval-required actions
(delete torrent, blocklist + search) must only appear in complete_diagnosis.escalate_action.

Max 3 autonomous actions before you must complete_diagnosis regardless.`

const (
	maxRounds            = 30
	maxAutonomousActions = 3
)

const (
	llmRetryDelay2 = 2 * time.Second
	llmRetryDelay3 = 4 * time.Second
)

// Agent orchestrates the LLM diagnostic loop for one incident.
type Agent struct {
	llm   *openai.Client
	model string
	disp  *Dispatcher
	db    *db.DB
	log   *slog.Logger
}

// New creates an Agent.
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
func (a *Agent) Run(
	ctx context.Context,
	inc *db.Incident,
	seed []openai.ChatCompletionMessage,
) (*DiagnosticResult, []openai.ChatCompletionMessage, error) {
	a.log.InfoContext(ctx, "starting diagnostic", "incident", inc.ID, "title", inc.Title)

	a.disp.IncidentID = inc.ID

	if err := a.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusInvestigating); err != nil {
		return nil, nil, err
	}

	messages := a.initMessages(inc, seed)
	tools := toolDefs()
	autonomousActions := 0
	seenCalls := make(map[string]int)

	for round := range maxRounds {
		resp, err := a.llmCall(ctx, openai.ChatCompletionRequest{
			Model:    a.model,
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return nil, messages, fmt.Errorf("llm round %d: %w", round, err)
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)

		if data, marshalErr := json.Marshal(messages); marshalErr == nil {
			_ = a.db.SaveConversation(ctx, inc.ID, json.RawMessage(data))
		}

		a.logTurn(ctx, inc.ID, round, msg)

		if len(msg.ToolCalls) == 0 {
			break
		}

		result, done, err := a.processToolCalls(ctx, inc, msg.ToolCalls, seenCalls, &autonomousActions, &messages)
		if err != nil {
			return nil, messages, err
		}
		if done {
			return result, messages, nil
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

// initMessages returns the starting message list for a run.
func (a *Agent) initMessages(inc *db.Incident, seed []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	if len(seed) > 0 {
		return seed
	}
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: a.buildUserMessage(inc)},
	}
}

// processToolCalls handles one batch of tool calls from the LLM.
// It appends tool result messages to *messages in place.
// Returns (result, true, nil) when complete_diagnosis is called or the action limit is hit.
func (a *Agent) processToolCalls(
	ctx context.Context,
	inc *db.Incident,
	calls []openai.ToolCall,
	seenCalls map[string]int,
	autonomousActions *int,
	messages *[]openai.ChatCompletionMessage,
) (*DiagnosticResult, bool, error) {
	for _, call := range calls {
		fn := call.Function.Name

		if fn == toolCompleteDiagnosis {
			result, err := a.handleCompleteDiagnosis(ctx, inc, call.Function.Arguments)
			if err != nil {
				return nil, false, err
			}
			return result, true, nil
		}

		if isAutonomousAction(fn) {
			*autonomousActions++
			_, _ = a.db.IncrementActionCount(ctx, inc.ID)

			if *autonomousActions > maxAutonomousActions {
				_ = a.db.SetAutonomousLocked(ctx, inc.ID, true)
				_ = a.db.UpdateIncidentStatus(ctx, inc.ID, db.StatusManualTestNeeded)
				return &DiagnosticResult{
					RootCause:        "max autonomous actions reached without resolution",
					Confidence:       "low",
					PrimaryAction:    "manual_investigation",
					PrimaryReason:    "agent applied 3 actions without confirming fix",
					RequiresApproval: true,
				}, true, nil
			}
		}

		resultJSON := a.executeCall(ctx, fn, call.Function.Arguments, seenCalls)
		*messages = append(*messages, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			Content:    resultJSON,
			ToolCallID: call.ID,
		})
	}
	return nil, false, nil
}

// handleCompleteDiagnosis parses the complete_diagnosis tool call arguments and
// runs post-fix verification when appropriate.
func (a *Agent) handleCompleteDiagnosis(
	ctx context.Context,
	inc *db.Incident,
	argsJSON string,
) (*DiagnosticResult, error) {
	var result DiagnosticResult
	if err := json.Unmarshal([]byte(argsJSON), &result); err != nil {
		return nil, fmt.Errorf("parse complete_diagnosis: %w", err)
	}
	if err := a.db.SetIncidentFinding(ctx, inc.ID, result, result); err != nil {
		a.log.ErrorContext(ctx, "set finding", "error", err)
	}

	if !result.RequiresApproval && inc.JellyfinItemID != "" {
		if !a.verifyFix(ctx, inc.JellyfinItemID) {
			a.log.WarnContext(ctx, "post-fix verification failed, escalating", "incident", inc.ID)
			result.RequiresApproval = true
			result.EscalateAction = "autonomous fix applied but playback verification failed"
		}
	}

	return &result, nil
}

// executeCall dispatches a single tool call with dedup protection for action tools.
func (a *Agent) executeCall(ctx context.Context, fn, argsJSON string, seenCalls map[string]int) string {
	callKey := fn + ":" + argsJSON
	seenCalls[callKey]++

	// Only block duplicate calls for state-changing actions. Read-only diagnostic
	// tools (playback info, dd, loki, etc.) may legitimately be re-called to
	// verify that an autonomous action actually fixed the problem.
	if isAutonomousAction(fn) && seenCalls[callKey] > 1 {
		a.log.WarnContext(ctx, "duplicate action blocked", "tool", fn)
		return jsonResult(map[string]any{
			keyError: fmt.Sprintf(
				"you already called %s — it has been applied, do not repeat it. Call complete_diagnosis with your current findings.",
				fn,
			),
		})
	}

	resultJSON := a.disp.Dispatch(ctx, fn, argsJSON)
	a.log.DebugContext(ctx, "tool call", "tool", fn, "result", resultJSON)
	return resultJSON
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

func (a *Agent) logTurn(ctx context.Context, incidentID string, round int, msg openai.ChatCompletionMessage) {
	b, _ := json.Marshal(msg)
	a.log.InfoContext(ctx, "agent_turn",
		"incident_id", incidentID,
		"round", round,
		"message", json.RawMessage(b),
	)
}

// llmCall wraps CreateChatCompletion with exponential backoff for transient errors.
func (a *Agent) llmCall(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	delays := []time.Duration{time.Second, llmRetryDelay2, llmRetryDelay3}
	var lastErr error
	for i, delay := range delays {
		if i > 0 {
			a.log.WarnContext(ctx, "llm transient error, retrying", "attempt", i, "error", lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return openai.ChatCompletionResponse{}, ctx.Err()
			}
		}
		resp, err := a.llm.CreateChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return openai.ChatCompletionResponse{}, fmt.Errorf("llm failed after %d attempts: %w", len(delays), lastErr)
}

// BuildSummarySeed constructs the seed messages for a resumed run from a prior-session summary.
func (a *Agent) BuildSummarySeed(inc *db.Incident, summary string) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf(
			"This is a resumed investigation of incident %q (type: %s).\n\nPrevious findings:\n%s\n\nContinue the diagnosis. Call complete_diagnosis when you have enough information.",
			inc.Title,
			inc.What,
			summary,
		)},
	}
}

func isAutonomousAction(toolName string) bool {
	switch toolName {
	case toolRefreshLinks, toolRepairSweep, toolRestartDecypharr,
		toolRestartJellyfin, toolSonarrRescan, toolRadarrRescan,
		toolClearJellyfinCache:
		return true
	}
	return false
}
