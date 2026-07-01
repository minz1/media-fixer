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

RESILIENCE: A tool returning an error, a 404, or an empty result is NEVER a reason to stop.
It is a single data point. For example, get_torrent_state returning no torrents just means
decypharr has no matching entry — keep going. Always continue the remaining steps and always
reach complete_diagnosis with whatever you found. Never abandon the whole diagnosis because
one tool call failed.

Media files live under /mnt/decypharr. Cache is at /var/cache/decypharr. Other data is at /data.
Library files under /data/library/{tv,movies}/ are SYMLINKS into /mnt/decypharr/__all__/<torrent>/.
list_directory reports each entry's is_symlink flag and its target — follow the target to find
the real file on the FUSE mount.

--- Playback problems (what=cant_play, missing_media) ---

Run ALL five steps before calling complete_diagnosis. Never bail out early.

Step 1 — Jellyfin lookup (always required).
  If the incident has a Jellyfin item ID, call jellyfin_playback_info with it directly.
  Exception: when source=seerr and details contains [media_type:tv], the item ID is the
  Series ID, not an episode. PlaybackInfo on a Series returns empty MediaSources — that is
  expected. Skip directly to step 2 and use the title/details to find the episode via
  torrent investigation. If S/E info appears in the title or details, use it to identify
  the right file in step 4.
  If there is no Jellyfin item ID, call jellyfin_search first. Searching strategy:
  a. Strip season/episode qualifiers and search the clean title
     ('the boys s1 episode 2' → 'the boys'; 'Breaking Bad S3E4' → 'Breaking Bad').
  b. If no results, try one looser variant: drop a leading article or use only the first word
     ('the boys' → 'boys'; 'stranger things' → 'stranger').
  c. If still no results after two attempts, skip jellyfin_playback_info and continue to step 2.
  Picking from search results: prefer an Episode whose season/episode matches the incident;
  if none, use the Series. Either way, always continue to step 2 — never stop here.

Step 2 — Disk check (always required).
  Call get_disk_info. Confirm /mnt/decypharr is mounted (mounted=true). Note: decypharr is a
  cloud-backed FUSE mount — total_bytes=0 with mounted=true is normal and does NOT mean the mount
  is down. Only mounted=false means the mount is absent.

Step 3 — Torrent state (always required).
  Call get_torrent_state with the show/movie name. Records decypharr's view of the torrent
  and gives you the torrent folder name for step 4.

Step 4 — File readability (always required).
  Determine the file path, then call dd_readability_test on it. Never pass a directory.
  - If jellyfin_playback_info returned MediaSources[].Path, use that path.
  - If MediaSources was empty or step 1 was skipped: call list_directory on
    /mnt/decypharr/<torrent-folder-from-step-3> to find the video file, then use that path.
  - If a path under /data/library is a symlink (is_symlink=true), dd-test its target
    (the /mnt/decypharr/__all__/... path), not the link itself.
  EIO errors or near-zero bytes-read confirm a FUSE/debrid link problem.

Step 5 — Log review (always required).
  Call loki_query with {unit=~"jellyfin|decypharr"} for the last 30 minutes.

After all five steps, call complete_diagnosis.

--- Jellyfin has the title but it is unplayable / a Series shows no episodes ---

Symptoms: jellyfin_playback_info on a Series returns empty MediaSources, OR loki_query shows
"InvalidCastException ... TV.Series ... IHasMediaSources" (Jellyfin tried to play a series
that has no episodes indexed). This is a Jellyfin indexing problem, NOT a debrid/FUSE problem,
when the underlying files ARE readable (Step 4 dd test passed).
  1. Call jellyfin_list_episodes on the Series item ID to confirm whether episodes are indexed.
  2. If empty, call clear_jellyfin_cache on the Series item ID (a recursive item refresh).
  3. Re-check with jellyfin_list_episodes. If episodes now appear, you are done.
  4. If still empty, escalate to a full library scan — but FIRST call jellyfin_scan_status.
     If a scan is already running, do NOT trigger another; just note its progress.
     Otherwise call jellyfin_library_scan once.
  5. A library scan takes minutes — do NOT wait in-run. Call complete_diagnosis with
     requires_approval=false, verify_after_seconds set to your estimate (e.g. 300-600), and
     user_eta_minutes set to a friendly "try again in N minutes". The system re-checks for you.

--- Infrastructure/connectivity problems (what=other, login_failed, or title is not a media title) ---

The report describes a service or connectivity issue, not a specific media item.

Step 1 — Always call loki_query for {unit=~"jellyfin|decypharr"} over the last 30 minutes.
  Look for: crashes, panics, OOM kills, repeated errors, failed mounts, auth failures,
  connection refused, or any ERROR/FATAL lines.

Step 2 — Always call get_disk_info to check mount health.
  /mnt/decypharr with total=0 means the FUSE mount is down.

Step 3 — Act on findings (apply the most appropriate action):
  - Jellyfin crashes / panics / not responding in logs → restart_jellyfin
  - decypharr errors, mount down (mounted=false), or decypharr stuck → restart_decypharr
  - Auth or login failures that are Jellyfin config issues → escalate (not autonomous)
  - No clear signal in logs or disk → escalate with a summary of what was checked

After both steps (and any action), call complete_diagnosis.

Once you have applied an autonomous action, call complete_diagnosis immediately — do not
keep querying logs or torrent state hoping to observe an async effect. When a fix needs time
to take hold (a library scan, a repair sweep, a refresh), do NOT escalate and do NOT wait
in-run: set requires_approval=false, verify_after_seconds to your best estimate, and
user_eta_minutes for the reporter. The system re-checks up to 5 times before deciding.

Never re-trigger a job that is already running. Check jellyfin_scan_status before
jellyfin_library_scan, and check get_repair_status before refresh_decypharr_links or
decypharr_repair_sweep — if a repair is already running, do NOT trigger another; wait via
verify_after_seconds instead. Re-triggering wastes time and confuses the user.

get_repair_health is a read-only diagnostic (it does not count as an action): use it to see
which specific entries decypharr considers broken, so you can target decypharr_recheck by name
instead of running a full sweep.

Action priority (least destructive first):
1. refresh_decypharr_links  — for EIO / stale CDN URLs
2. decypharr_recheck        — recheck one specific broken entry by name
3. decypharr_repair_sweep   — general broken-entry check
4. decypharr_cache_cleanup  — FUSE mount serving stale paths (EIO through mount, debrid link OK)
5. clear_jellyfin_cache     — stale metadata, or a Series with no episodes indexed (recursive refresh)
6. jellyfin_library_scan    — items exist on disk but are not indexed (check scan_status first)
7. restart_decypharr        — if decypharr appears stuck or FUSE mount is down
8. restart_jellyfin         — if Jellyfin logs show crashes or it is unresponsive
9. sonarr_rescan / radarr_rescan — if Jellyfin sees no sources but file might be present

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
	// VerifyAfterSeconds, when > 0, tells the system a non-destructive fix was
	// applied that needs time (e.g. a library scan). The system re-checks whether
	// the problem resolved instead of escalating immediately.
	VerifyAfterSeconds int `json:"verify_after_seconds,omitempty"`
	// VerifyItemID is the Jellyfin item to re-check; defaults to the incident's item.
	VerifyItemID string `json:"verify_item_id,omitempty"`
	// UserETAMinutes is the agent's friendly estimate for when the reporter should
	// try again, used in the "should be fixed soon" notification.
	UserETAMinutes int `json:"user_eta_minutes,omitempty"`
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

	// When the agent requested deferred verification (verify_after_seconds > 0),
	// the fix needs time to take effect — skip the instant check and let the
	// service's verification loop re-check after the requested delay.
	if !result.RequiresApproval && result.VerifyAfterSeconds == 0 && inc.JellyfinItemID != "" {
		if !a.VerifyResolved(ctx, inc.JellyfinItemID) {
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

// VerifyResolved reports whether a playback problem looks resolved for an item:
// either Jellyfin can now open it (PlaybackInfo has media sources) or, for a
// series, episodes are now indexed.
func (a *Agent) VerifyResolved(ctx context.Context, itemID string) bool {
	if info, err := a.disp.Jellyfin.PlaybackInfo(ctx, itemID); err == nil && len(info.MediaSources) > 0 {
		return true
	}
	if eps, err := a.disp.Jellyfin.ListEpisodes(ctx, itemID); err == nil && len(eps) > 0 {
		return true
	}
	return false
}

// ScanRunning reports whether a Jellyfin library scan is currently in progress.
func (a *Agent) ScanRunning(ctx context.Context) bool {
	st, err := a.disp.Jellyfin.ScanStatus(ctx)
	return err == nil && st.Running
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
			"This is a resumed investigation of incident %q (type: %s).\n\nPrevious findings:\n%s\n\n"+
				"Continue the diagnosis. Before calling complete_diagnosis you MUST call loki_query and "+
				"get_disk_info to refresh current state — conditions may have changed since the prior run. "+
				"Do not skip these even if the summary already contains similar data.",
			inc.Title,
			inc.What,
			summary,
		)},
	}
}

func isAutonomousAction(toolName string) bool {
	switch toolName {
	case toolRefreshLinks, toolRepairSweep, toolCacheCleanup, toolDecypharrRecheck,
		toolRestartDecypharr, toolRestartJellyfin, toolSonarrRescan, toolRadarrRescan,
		toolClearJellyfinCache, toolJellyfinLibraryScan:
		return true
	}
	return false
}
