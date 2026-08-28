package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
  Series ID, not an episode. PlaybackInfo on a Series ID throws an error on some Jellyfin
  builds (InvalidCastException casting Series to IHasMediaSources) rather than returning
  empty MediaSources — confirmed happening even when the series has episodes indexed, so
  don't read anything diagnostic into that error either way. Skip directly to step 2 and use
  the title/details to find the episode via torrent investigation. If S/E info appears in the
  title or details, use it to identify the right file in step 4.
  If there is no Jellyfin item ID, call jellyfin_search first. Searching strategy:
  a. Strip season/episode qualifiers and search the clean title
     ('the boys s1 episode 2' → 'the boys'; 'Breaking Bad S3E4' → 'Breaking Bad').
  b. If no results, try one looser variant: drop a leading article or use only the first word
     ('the boys' → 'boys'; 'stranger things' → 'stranger').
  c. If still no results after two attempts, skip jellyfin_playback_info and continue to step 2.
  Picking from search results: prefer an Episode whose season/episode matches the incident;
  if none, use the Series. Either way, always continue to step 2 — never stop here.

Step 2 — Disk check (always required).
  Call get_disk_info. Each path reports two independent booleans, accessible and is_mount_point:
  - /mnt/decypharr is a FUSE mount and MUST be a mount point. Healthy = accessible:true AND
    is_mount_point:true (total_bytes:0 is normal here — it is cloud-backed, not empty). If
    is_mount_point:false, the FUSE mount has DIED and fallen back to an empty directory on the
    root FS — it looks healthy (accessible:true, non-zero bytes) but is actually down. If
    accessible:false with is_mount_point:true, the mount entry is stale and the FUSE daemon is dead.
  - /data and /var/cache/decypharr may legitimately NOT be separate mounts, so is_mount_point:false
    there is normal — only accessible:false indicates a problem for those.

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
  Call loki_query with {unit=~"jellyfin.service|decypharr.service"} for the last 30 minutes, and
  set filter to the title (or the filename from step 1/4) so you only see lines about THIS item.
  Without a filter you get every line from both services in the window, including ones about a
  completely different title — those are not evidence about this incident and must not be cited
  as the root cause, no matter how relevant-sounding the error looks.

After all five steps, call complete_diagnosis.

--- Jellyfin has the title but it is unplayable / a Series shows no episodes ---

Symptoms: jellyfin_list_episodes on the Series item ID returns empty. (loki_query may also show
"InvalidCastException ... TV.Series ... IHasMediaSources" around playback attempts, but that
exception alone is NOT diagnostic — on some Jellyfin builds calling jellyfin_playback_info
directly on a Series ID throws this every time, indexed or not. Use jellyfin_list_episodes,
not the exception, to confirm indexing is actually the problem.) This is a Jellyfin indexing
problem, NOT a debrid/FUSE problem, when the underlying files ARE readable (Step 4 dd test
passed).
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

Step 1 — Always call loki_query for {unit=~"jellyfin.service|decypharr.service"} over the last 30
  minutes. Leave filter unset here — there is no specific item, so an unfiltered scan for
  crashes, panics, OOM kills, repeated errors, failed mounts, auth failures, connection refused,
  or any ERROR/FATAL lines is the right approach for a service-level problem.

Step 2 — Always call get_disk_info to check mount health.
  /mnt/decypharr with is_mount_point:false means the FUSE mount is down (fell back to an empty
  root-FS dir) even if accessible:true and byte counts look normal. accessible:false also = down.

Step 3 — Act on findings (apply the most appropriate action):
  - Jellyfin crashes / panics / not responding in logs → restart_jellyfin
  - decypharr errors, mount down (/mnt/decypharr is_mount_point=false or accessible=false), or decypharr stuck → restart_decypharr
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

If your user message includes a "Actions already attempted on this incident" section: those
actions were applied successfully but did NOT resolve the problem — this incident is still open
after them. That is evidence AGAINST the diagnosis that led to them, not a reason to retry. If
your current evidence points toward an action already in that list, do not call it again (a
repeat is blocked and returns an error) — move to the next-lower-priority action below, or set
escalate_action to manual_investigation. Retrying the same fix on the same evidence wastes a
turn and delays the reporter for nothing.

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

You may call autonomous actions directly. Approval-required actions must only appear in
complete_diagnosis.escalate_action, never called as a tool yourself.

escalate_action must be one of: none | remove_and_search | manual_investigation.
  - remove_and_search: the file(s) on disk are wrong/corrupt and Sonarr/Radarr should delete
    them, blocklist the release that produced them, and search for a replacement. Use this
    when dd_readability_test or get_torrent_state points at a specific bad file/release
    (not a FUSE/mount/service problem — those are refresh_decypharr_links, restart_decypharr,
    etc., which you call directly). When you set this, you MUST also set escalate_params:
    media_type ("tv"|"movie"), title, scope ("episode"|"season"|"series", tv only), season
    and episode (ints, when scope needs them), blocklist (bool, default true). Be as specific
    as the evidence allows — prefer scope=episode over season or series when you know which
    episode is bad.
  - manual_investigation: you cannot form a confident diagnosis or fix; the owner needs to look.
  - none: no escalation needed (requires_approval should be false in this case).

This deletes files and triggers a re-download — it is owner-approved only. Never claim you
performed it; you only recommend it.

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
	RootCause      string `json:"root_cause"`
	Confidence     string `json:"confidence"`
	PrimaryAction  string `json:"primary_action"`
	PrimaryReason  string `json:"primary_reason"`
	FallbackAction string `json:"fallback_action,omitempty"`
	// EscalateAction is one of the escalateActionEnum values (see tools.go) —
	// an owner-approval-required action the agent could not apply itself.
	EscalateAction string `json:"escalate_action,omitempty"`
	// EscalateParams carries the target for EscalateAction (e.g. media_type,
	// title, scope, season, episode, blocklist for remove_and_search). Passed
	// straight through to Agent.PlanEscalation/RunEscalation.
	EscalateParams   map[string]any `json:"escalate_params,omitempty"`
	RequiresApproval bool           `json:"requires_approval"`
	// VerifyAfterSeconds, when > 0, tells the system a non-destructive fix was
	// applied that needs time (e.g. a library scan). The system re-checks whether
	// the problem resolved instead of escalating immediately.
	VerifyAfterSeconds int `json:"verify_after_seconds,omitempty"`
	// VerifyItemID is the Jellyfin item to re-check; defaults to the incident's item.
	VerifyItemID string `json:"verify_item_id,omitempty"`
	// UserETAMinutes is the agent's friendly estimate for when the reporter should
	// try again, used in the "should be fixed soon" notification.
	UserETAMinutes int `json:"user_eta_minutes,omitempty"`
	// PreFix is a snapshot of the target item's observable state captured at the
	// start of Run, before any tool executed. It is set by code (Run/
	// handleCompleteDiagnosis), never by the model — complete_diagnosis's tool
	// schema has no pre_fix parameter, so any same-named key the model happened
	// to send is unmarshaled here first and then unconditionally overwritten.
	// VerifyResolved compares it against a fresh capture to confirm something
	// actually changed, not just that the item currently looks healthy.
	PreFix *FixSignature `json:"pre_fix,omitempty"`
}

// FixSignature is a snapshot of a Jellyfin item's observable playback state:
// whether it's indexed, and whether the file it currently points at is
// readable. VerifyResolved requires this to differ before and after a fix —
// without that, "verified" meant only "Jellyfin still reports a source",
// which is true for nearly any indexed item whether or not it actually
// plays, and is why every fix claim in a week of production logs "verified"
// even on incidents that came right back.
type FixSignature struct {
	Path         string `json:"path,omitempty"`
	SourceCount  int    `json:"source_count"`
	EpisodeCount int    `json:"episode_count"`
	DDBytesRead  int64  `json:"dd_bytes_read"`
	DDError      string `json:"dd_error,omitempty"`
}

// escalationLabel gives a short human-readable phrase for an
// escalateActionEnum value, used in owner-facing notifications.
func escalationLabel(action string) string {
	switch action {
	case EscalateNone:
		return "no action"
	case EscalateRemoveAndSearch:
		return "remove file(s) and re-search"
	case EscalateManualInvestigation:
		return "manual investigation needed"
	default:
		return action
	}
}

// EscalationSummary composes a one-line human-readable description of a
// recommended escalation for owner notifications: a short label for
// EscalateAction, plus the target media from EscalateParams when present.
func EscalationSummary(result *DiagnosticResult) string {
	label := escalationLabel(result.EscalateAction)
	title, _ := result.EscalateParams[paramTitle].(string)
	if title == "" {
		return label
	}

	parts := []string{title}
	if scope, hasScope := result.EscalateParams[paramScope].(string); hasScope && scope != "" {
		parts = append(parts, scope)
	}
	if season, hasSeason := result.EscalateParams[paramSeason].(float64); hasSeason {
		parts = append(parts, fmt.Sprintf("S%02d", int(season)))
	}
	if episode, hasEpisode := result.EscalateParams[paramEpisode].(float64); hasEpisode {
		parts = append(parts, fmt.Sprintf("E%02d", int(episode)))
	}
	return fmt.Sprintf("%s (%s)", label, strings.Join(parts, " "))
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
	a.disp.IncidentTime = inc.CreatedAt

	// Gate on the transition (not a raw status write) so a stale, superseded run
	// can never clobber a finished incident's status back to "investigating" if it
	// happens to reach this point after a newer run already completed. AgentFixed
	// and ManualTestNeeded are included so a rerun (Service.Rerun, which sets
	// status to Reopened before launching) still succeeds even if that write is
	// lost to a race — without them, a stale terminal status here left the
	// "Re-run diagnosis" button applying no-op runs, silently swallowed further
	// downstream by runVerification's own gate never allowing a terminal status in.
	changed, claimErr := a.db.TransitionStatus(ctx, inc.ID, db.StatusInvestigating,
		db.StatusOpen, db.StatusInvestigating, db.StatusVerifying, db.StatusReopened,
		db.StatusAgentFixed, db.StatusManualTestNeeded)
	if claimErr != nil {
		return nil, nil, claimErr
	}
	if !changed {
		a.log.WarnContext(ctx, "run could not claim incident; status changed before start", "incident", inc.ID)
		return nil, nil, ErrIncidentNotInvestigatable
	}

	// Snapshot the target item's state before any tool runs, so a fix can be
	// verified against what actually changed rather than just how healthy the
	// item looks afterward. Captured once per run (not persisted across a
	// later Rerun) — "did what this run's action changed" is the question that
	// matters, and a fresh Run already gets a fresh baseline.
	preFix := a.captureSignature(ctx, inc.JellyfinItemID)

	messages := a.initMessages(ctx, inc, seed)
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

		result, done, err := a.processToolCalls(
			ctx,
			inc,
			preFix,
			msg.ToolCalls,
			seenCalls,
			&autonomousActions,
			&messages,
		)
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
func (a *Agent) initMessages(
	ctx context.Context,
	inc *db.Incident,
	seed []openai.ChatCompletionMessage,
) []openai.ChatCompletionMessage {
	if len(seed) > 0 {
		return seed
	}
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: a.buildUserMessage(ctx, inc)},
	}
}

// processToolCalls handles one batch of tool calls from the LLM.
// It appends tool result messages to *messages in place.
// Returns (result, true, nil) when complete_diagnosis is called or the action limit is hit.
func (a *Agent) processToolCalls(
	ctx context.Context,
	inc *db.Incident,
	preFix *FixSignature,
	calls []openai.ToolCall,
	seenCalls map[string]int,
	autonomousActions *int,
	messages *[]openai.ChatCompletionMessage,
) (*DiagnosticResult, bool, error) {
	for _, call := range calls {
		fn := call.Function.Name

		if fn == toolCompleteDiagnosis {
			result, err := a.handleCompleteDiagnosis(ctx, inc, preFix, call.Function.Arguments)
			if err != nil {
				return nil, false, err
			}
			return result, true, nil
		}

		if isAutonomousAction(fn) {
			// Structural backstop for the "do not repeat a failed action" instruction
			// in attemptHistory: the prompt is not enough on its own — a run seeded
			// from a lossy summary, or one that just weighs the evidence the same way
			// again, can still reach for the same fix a second time. This blocks it
			// before it executes rather than trusting the model to have read the
			// history. Checked (and messaged) before the action-limit increment below
			// so a blocked repeat doesn't burn from the same budget a real attempt
			// would.
			if a.actionAlreadyApplied(ctx, inc.ID, fn, call.Function.Arguments) {
				a.log.WarnContext(ctx, "duplicate action blocked, already applied on this incident", "tool", fn)
				*messages = append(*messages, openai.ChatCompletionMessage{
					Role: openai.ChatMessageRoleTool,
					Content: jsonResult(map[string]any{
						keyError: fmt.Sprintf(
							"%s (with these parameters) was already applied on this incident and did not "+
								"resolve it. Do not repeat it — choose a different action, or set "+
								"escalate_action and call complete_diagnosis.",
							fn,
						),
					}),
					ToolCallID: call.ID,
				})
				continue
			}

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
	preFix *FixSignature,
	argsJSON string,
) (*DiagnosticResult, error) {
	var result DiagnosticResult
	if err := json.Unmarshal([]byte(argsJSON), &result); err != nil {
		return nil, fmt.Errorf("parse complete_diagnosis: %w", err)
	}
	// The model has no route to set this — complete_diagnosis's tool schema has
	// no pre_fix parameter — but assign it unconditionally regardless, so a
	// same-named key it happened to send is never trusted over the real capture.
	result.PreFix = preFix

	itemID := result.VerifyItemID
	if itemID == "" {
		itemID = inc.JellyfinItemID
	}

	// When the agent requested deferred verification (verify_after_seconds > 0),
	// the fix needs time to take effect — skip the instant check and let the
	// service's verification loop re-check after the requested delay.
	if !result.RequiresApproval && result.VerifyAfterSeconds == 0 && itemID != "" {
		if !a.VerifyResolved(ctx, itemID, preFix) {
			a.log.WarnContext(ctx, "post-fix verification failed, escalating", "incident", inc.ID)
			result.RequiresApproval = true
			result.EscalateAction = EscalateManualInvestigation
			result.PrimaryReason = "autonomous fix applied but playback verification failed; " + result.PrimaryReason
		}
	}

	if err := a.db.SetIncidentFinding(ctx, inc.ID, result, result); err != nil {
		a.log.ErrorContext(ctx, "set finding", "error", err)
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

// captureSignature snapshots an item's current observable state: what
// Jellyfin reports for it right now, and whether the path Jellyfin currently
// points at (which can change between runs — the underlying file may be
// replaced) is actually readable. Best-effort: every failure is recorded in
// the signature rather than returned as an error, since a snapshot must never
// abort a diagnosis. Returns a zero-value signature for an empty itemID
// (common on a freshly-reported incident whose item hasn't been resolved
// yet) — VerifyResolved treats that as "no baseline", not as "unchanged".
func (a *Agent) captureSignature(ctx context.Context, itemID string) *FixSignature {
	sig := &FixSignature{}
	if itemID == "" || a.disp == nil {
		return sig
	}

	if info, err := a.disp.Jellyfin.PlaybackInfo(ctx, itemID); err == nil {
		sig.SourceCount = len(info.MediaSources)
		if sig.SourceCount > 0 {
			sig.Path = info.MediaSources[0].Path
		}
	}
	if eps, err := a.disp.Jellyfin.ListEpisodes(ctx, itemID); err == nil {
		sig.EpisodeCount = len(eps)
	}

	if sig.Path != "" && a.disp.MediaAgent != nil {
		if dd, err := a.disp.MediaAgent.DDReadabilityTest(ctx, sig.Path); err == nil {
			sig.DDBytesRead = dd.BytesRead
			sig.DDError = dd.Error
		} else {
			sig.DDError = err.Error()
		}
	}
	return sig
}

// VerifyResolved reports whether a playback problem looks resolved for an
// item. It requires evidence the item is currently usable — a movie/episode
// needs a real, successful dd read of whatever path Jellyfin is reporting
// right now (not the path recorded at diagnosis time); a series needs
// episodes indexed, since PlaybackInfo throws on a Series ID on some Jellyfin
// builds regardless of indexing state (see systemPrompt) and can't be used
// there — AND, when pre is non-nil, that the current signature actually
// differs from it. An identical signature means nothing observably changed
// as a result of whatever was just applied, which used to pass this check as
// long as the item looked healthy at all — true for nearly any indexed item
// whether or not it actually plays, which is why every fix claim in a week
// of production logs "verified" even on incidents that came right back.
func (a *Agent) VerifyResolved(ctx context.Context, itemID string, pre *FixSignature) bool {
	post := a.captureSignature(ctx, itemID)

	sourceOK := post.SourceCount > 0 && post.Path != "" && post.DDBytesRead > 0 && post.DDError == ""
	episodesOK := post.EpisodeCount > 0
	if !sourceOK && !episodesOK {
		return false
	}
	return pre == nil || *post != *pre
}

// ErrIncidentNotInvestigatable is returned by Run when the incident's status
// was not one Run is allowed to claim (e.g. it was concurrently resolved or
// blocked) — the incident changed out from under this run before it could
// start. The caller treats this the same as a superseded run: exit quietly,
// do not escalate to the owner.
var ErrIncidentNotInvestigatable = errors.New("incident not in an investigatable status")

// errNoEscalationPlan is returned by PlanEscalation/RunEscalation for
// escalate_action values that have no automated preview or execution — the
// owner must act on them manually from the diagnosis alone.
var errNoEscalationPlan = errors.New("escalate_action has no automated plan; act manually")

// PlanEscalation resolves what RunEscalation would do for a diagnostic
// result's recommended escalation, without making any changes. Used by the
// dashboard's Preview button and by live-check tooling.
func (a *Agent) PlanEscalation(ctx context.Context, result *DiagnosticResult) (any, error) {
	switch result.EscalateAction {
	case EscalateRemoveAndSearch:
		return a.disp.readArrRemoveAndSearchPlan(ctx, result.EscalateParams)
	default:
		return nil, fmt.Errorf("%w: %q", errNoEscalationPlan, result.EscalateAction)
	}
}

// RunEscalation executes a diagnostic result's recommended escalation after
// owner approval. It re-resolves the plan against current state rather than
// trusting a possibly-stale preview.
func (a *Agent) RunEscalation(ctx context.Context, result *DiagnosticResult) (any, error) {
	switch result.EscalateAction {
	case EscalateRemoveAndSearch:
		return a.disp.executeArrRemoveAndSearch(ctx, result.EscalateParams)
	default:
		return nil, fmt.Errorf("%w: %q", errNoEscalationPlan, result.EscalateAction)
	}
}

// ScanRunning reports whether a Jellyfin library scan is currently in progress.
func (a *Agent) ScanRunning(ctx context.Context) bool {
	st, err := a.disp.Jellyfin.ScanStatus(ctx)
	return err == nil && st.Running
}

func (a *Agent) buildUserMessage(ctx context.Context, inc *db.Incident) string {
	return fmt.Sprintf(`New incident reported.
Title: %s
Problem type: %s
Source: %s
Reported by: %s
Details: %s
Jellyfin item ID: %s
Report time: %s
%s
Please diagnose the root cause and apply the least-destructive fix(es) autonomously.
Call complete_diagnosis when done.`,
		inc.Title,
		inc.What,
		inc.Source,
		inc.ReportedBy,
		inc.Details,
		inc.JellyfinItemID,
		inc.CreatedAt.Format(time.RFC3339),
		a.attemptHistory(ctx, inc.ID),
	)
}

// attemptHistory renders every action already logged against this incident, so
// a resumed or rerun diagnosis knows what has already been tried instead of
// re-discovering — and re-applying — the same fix from a blank slate. Every
// logged action was successfully applied to the system (Dispatcher.logAction
// only fires after the underlying call succeeds), and the fact that Run is
// being invoked again at all means the incident is still open, so "did not
// resolve it" is a safe inference, not a guess. Returns "" when nothing has
// been logged yet, the common case for a brand-new incident.
func (a *Agent) attemptHistory(ctx context.Context, incidentID string) string {
	actions, err := a.db.ListActions(ctx, incidentID)
	if err != nil || len(actions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nActions already attempted on this incident — every one of these was applied " +
		"successfully but did NOT resolve the problem (the incident is still open). Do not repeat any " +
		"of them; if your evidence points toward one again, that is a signal to look for a different " +
		"cause or to escalate, not to retry it:\n")
	for _, act := range actions {
		params, _ := json.Marshal(act.Params)
		when := "unknown time"
		if act.AppliedAt != nil {
			when = act.AppliedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(&sb, "  - %s %s (applied %s)\n", act.Action, params, when)
	}
	return sb.String()
}

// identityParamsMatch reports whether a and b identify the same target: both
// empty (a parameterless action like restart_jellyfin), or sharing a value
// for one of the tool argument keys used across the registry to identify
// *what* an action targets. Checked individually instead of comparing full
// JSON objects because logAction sometimes records extra resolved fields the
// LLM's own call arguments never contain — e.g. sonarr_rescan logs
// series_id alongside the title the LLM passed.
func identityParamsMatch(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	identityParamKeys := []string{paramItemID, paramName, paramTitle, paramSeriesID, paramMovieID}
	for _, k := range identityParamKeys {
		av, aok := a[k]
		bv, bok := b[k]
		if aok && bok {
			return fmt.Sprint(av) == fmt.Sprint(bv)
		}
	}
	return false
}

// actionAlreadyApplied reports whether this exact action — same tool name,
// same identifying target — has already been logged as successfully applied
// on this incident.
func (a *Agent) actionAlreadyApplied(ctx context.Context, incidentID, action, argsJSON string) bool {
	actions, err := a.db.ListActions(ctx, incidentID)
	if err != nil {
		return false
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &args)
	for _, act := range actions {
		if act.Action != action || act.Status != db.ActionApplied {
			continue
		}
		params, _ := act.Params.(map[string]any)
		if identityParamsMatch(args, params) {
			return true
		}
	}
	return false
}

func (a *Agent) logTurn(ctx context.Context, incidentID string, round int, msg openai.ChatCompletionMessage) {
	b, _ := json.Marshal(msg)
	a.log.InfoContext(ctx, "agent_turn",
		"incident_id", incidentID,
		"round", round,
		"message", json.RawMessage(b),
	)
	// Best-effort: a run that's still making progress touches its heartbeat once
	// per round, so a sweeper can tell "hung" (process alive, stuck) apart from
	// "just slow" without waiting for the whole diagnostic loop to time out.
	_ = a.db.TouchHeartbeat(ctx, incidentID)
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
func (a *Agent) BuildSummarySeed(ctx context.Context, inc *db.Incident, summary string) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf(
			"This is a resumed investigation of incident %q (type: %s).\n\nPrevious findings:\n%s\n%s\n\n"+
				"Continue the diagnosis. Before calling complete_diagnosis you MUST call loki_query and "+
				"get_disk_info to refresh current state — conditions may have changed since the prior run. "+
				"Do not skip these even if the summary already contains similar data.",
			inc.Title,
			inc.What,
			summary,
			a.attemptHistory(ctx, inc.ID),
		)},
	}
}
