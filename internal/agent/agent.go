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

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	"github.com/minz1/mediafixer/internal/journal"
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

If your user message includes a "NOTE: <action> was run ... while investigating incident #..."
line: a different incident's service-wide action may still be settling. Do NOT diagnose a root
cause from evidence gathered inside that window — an EIO, a connection-refused, or a missing
path seen right now could be a transient side effect of that disruption, not a fact about this
incident. Prefer re-checking (a fresh dd_readability_test, disk check, etc.) over concluding.

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
  EIO errors or near-zero bytes-read on a file that DOES exist confirm a FUSE/debrid link
  problem. dd_readability_test's not_found:true (the file, or its parent directory via
  list_directory, does not exist at all — "no such file or directory") is a DIFFERENT finding
  and must never be treated as EIO: the content is not there, which usually means it was never
  fully downloaded, not that the mount is broken. On not_found:true, call arr_media_status for
  this title (with season/episode if known) BEFORE concluding anything about decypharr/FUSE. If
  it reports has_file=false, that IS the root cause — use arr_search_missing, not a
  decypharr/Jellyfin action.
  If arr_media_status shows has_file=false and you want to know WHY (never grabbed vs. grabbed
  but the import failed), call arr_grab_history — the fix differs (arr_search_missing for
  "never grabbed"; escalate_action=remove_and_search if a bad file WAS imported).

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

--- Content is missing entirely (arr_media_status confirms has_file=false) ---

This is the fix when Step 4 above found the file (or its containing directory) does not exist,
AND arr_media_status confirmed has_file=false for the specific target. This is NOT a
decypharr/Jellyfin problem — nothing was ever downloaded (or a prior download was later
removed). Do not reach for clear_jellyfin_cache, jellyfin_library_scan, decypharr_cache_cleanup,
or any restart here; none of them can produce a file that was never downloaded.
  1. Call arr_search_missing with the narrowest target you can identify (episode > season >
     series). It is refused if arr_media_status was wrong and a file actually exists — in that
     case use escalate_action=remove_and_search instead (owner-approved; the file is bad, not
     missing).
  2. This kicks off an asynchronous search + download — do NOT wait in-run. Call
     complete_diagnosis with requires_approval=false and verify_after_seconds set generously
     (a download takes far longer than a library scan or cache refresh — prefer 1800+), with
     user_eta_minutes reflecting that (e.g. 20-30). The system tracks it through to completion.

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
repeat is blocked and returns an error).

A DIFFERENT action is only justified by evidence YOU observed in this run that points at a
different cause than the one already tried — not by mechanically moving to the next item in the
priority list below. The priority list ranks actions by how destructive they are, given a cause;
it is not a queue to work through when one fix fails. If nothing you've found in this run points
at a specific different cause, set escalate_action to manual_investigation instead of guessing —
a plausible-sounding but unevidenced escalation to a more disruptive action (e.g. a restart after
a cache clear failed, with no new evidence a restart would help) is worse than asking for help,
and control review will likely catch it anyway once two or more actions have been tried.

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
	llm     *openai.Client
	model   string
	disp    *Dispatcher
	db      *db.DB
	journal *journal.Journal
	log     *slog.Logger
}

// New creates an Agent.
func New(
	llm *openai.Client, model string, disp *Dispatcher, database *db.DB, jrnl *journal.Journal, log *slog.Logger,
) *Agent {
	return &Agent{llm: llm, model: model, disp: disp, db: database, journal: jrnl, log: log}
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

// FixSignature is a snapshot of a title's observable state: Jellyfin's
// indexing/playback view, and — best-effort — *arr's view of whether it
// actually has a file at all. VerifyResolved requires post to be a strict
// improvement over pre, not just different — without that, "verified" meant
// only "Jellyfin still reports a source" (true for nearly any indexed item
// whether or not it actually plays) or "the signature isn't byte-identical"
// (true even when a count went down, not up), and is why every fix claim in
// a week of production logs "verified" even on incidents that came right
// back, including one where the specific missing episode never got a file
// but the series' other 9 episodes staying indexed was enough to pass.
type FixSignature struct {
	Path         string `json:"path,omitempty"`
	SourceCount  int    `json:"source_count"`
	EpisodeCount int    `json:"episode_count"`
	DDBytesRead  int64  `json:"dd_bytes_read"`
	DDError      string `json:"dd_error,omitempty"`
	// ArrChecked/ArrHasFile carry *arr's aggregate "has every file" answer
	// for the incident's title (see Agent.arrHasFileForTitle) — plain bools
	// rather than a *bool so FixSignature stays comparable by value (a
	// pointer field would make two equal-valued signatures compare unequal
	// by address, silently defeating every comparison below). ArrHasFile is
	// only meaningful when ArrChecked is true; unchecked (title didn't
	// resolve against Sonarr/Radarr, or neither is configured) never blocks
	// verification — it just means this signal isn't available.
	ArrChecked bool `json:"arr_checked,omitempty"`
	ArrHasFile bool `json:"arr_has_file,omitempty"`
}

// ParseDiagnosticResult re-decodes an incident's stored finding — persisted
// generically as `any` by db.SetIncidentFinding, so db.Incident.Finding comes
// back as a map[string]any, not a *DiagnosticResult — into its typed form,
// for rendering a structured report instead of a raw JSON dump. Returns
// (nil, false) on any decode failure (an old row in an unexpected shape, or
// simply no finding yet) rather than a partially-populated struct, so the
// caller can cleanly fall back to the raw view instead of rendering
// zero-valued fields as if they meant something.
func ParseDiagnosticResult(finding any) (*DiagnosticResult, bool) {
	if finding == nil {
		return nil, false
	}
	b, err := json.Marshal(finding)
	if err != nil {
		return nil, false
	}
	var result DiagnosticResult
	if unmarshalErr := json.Unmarshal(b, &result); unmarshalErr != nil {
		return nil, false
	}
	return &result, true
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
	a.recordStatusChanged(ctx, inc.ID, string(db.StatusInvestigating))

	// Snapshot the target item's state before any tool runs, so a fix can be
	// verified against what actually changed rather than just how healthy the
	// item looks afterward. Captured once per run (not persisted across a
	// later Rerun) — "did what this run's action changed" is the question that
	// matters, and a fresh Run already gets a fresh baseline.
	preFix := a.captureSignature(ctx, inc.JellyfinItemID, inc.Title)

	messages := a.initMessages(ctx, inc, seed)
	a.recordRunStarted(ctx, inc.ID, messages)

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
			a.recordRunFinished(ctx, inc.ID, round, err)
			return nil, messages, fmt.Errorf("llm round %d: %w", round, err)
		}

		msg := resp.Choices[0].Message
		messages = append(messages, msg)
		a.logTurn(ctx, inc.ID, round, msg)

		if len(msg.ToolCalls) == 0 {
			a.recordRound(ctx, inc.ID, round, msg, nil)
			break
		}

		toolMsgStart := len(messages)
		result, done, err := a.processToolCalls(
			ctx,
			inc,
			preFix,
			msg.ToolCalls,
			seenCalls,
			&autonomousActions,
			&messages,
		)
		// Recorded once per round, AFTER tool calls are processed — not, as the
		// old per-round SaveConversation did, right after the assistant message
		// and before this. That ordering was the reason the final round's tool
		// results (the ones complete_diagnosis itself triggers no further round
		// to re-save) were never persisted at all, and every other round's
		// landed one round late.
		a.recordRound(ctx, inc.ID, round, msg, messages[toolMsgStart:])
		if err != nil {
			a.recordRunFinished(ctx, inc.ID, round, err)
			return nil, messages, err
		}
		if done {
			a.recordRunFinished(ctx, inc.ID, round, nil)
			return result, messages, nil
		}
	}

	exhausted := &DiagnosticResult{
		RootCause:        "diagnostic loop exhausted without conclusion",
		Confidence:       "low",
		PrimaryAction:    "manual_investigation",
		PrimaryReason:    "agent did not reach a conclusion within iteration limit",
		RequiresApproval: true,
	}
	a.recordRunFinished(ctx, inc.ID, maxRounds, errLoopExhausted)
	return exhausted, messages, nil
}

// errLoopExhausted labels a run_finished event whose loop ran out of rounds
// without complete_diagnosis ever being called — distinct from a genuine LLM
// or tool error, but still worth recording as non-nil so the transcript shows
// it wasn't a clean exit.
var errLoopExhausted = errors.New("diagnostic loop exhausted without conclusion")

// recordRunStarted, recordRound, and recordRunFinished are best-effort:
// journal writes must never abort a diagnosis, and a.journal is nil in
// contexts that don't care about the durable transcript (e.g. some tests
// construct an Agent without one).
func (a *Agent) recordRunStarted(ctx context.Context, incidentID string, seed []openai.ChatCompletionMessage) {
	if a.journal == nil {
		return
	}
	if err := a.journal.RunStarted(ctx, incidentID, seed); err != nil {
		a.log.WarnContext(ctx, "record run_started", "incident", incidentID, "error", err)
	}
}

func (a *Agent) recordRound(
	ctx context.Context, incidentID string, round int,
	assistant openai.ChatCompletionMessage, toolMessages []openai.ChatCompletionMessage,
) {
	if a.journal == nil {
		return
	}
	if err := a.journal.LLMRound(ctx, incidentID, round, assistant, toolMessages); err != nil {
		a.log.WarnContext(ctx, "record llm_round", "incident", incidentID, "round", round, "error", err)
	}
}

func (a *Agent) recordRunFinished(ctx context.Context, incidentID string, rounds int, runErr error) {
	if a.journal == nil {
		return
	}
	if err := a.journal.RunFinished(ctx, incidentID, rounds, runErr); err != nil {
		a.log.WarnContext(ctx, "record run_finished", "incident", incidentID, "error", err)
	}
}

// recordStatusChanged notifies live subscribers (see incidentEvents in
// internal/server) that this incident's status moved, for the two status
// transitions Agent itself performs directly (claiming the incident into
// "investigating", and the max-autonomous-actions escalation) — every other
// transition happens one layer up in Service, which has its own identical
// helper for the same reason.
func (a *Agent) recordStatusChanged(ctx context.Context, incidentID, status string) {
	if a.journal == nil {
		return
	}
	if err := a.journal.StatusChanged(ctx, incidentID, status); err != nil {
		a.log.WarnContext(ctx, "record status_changed", "incident", incidentID, "error", err)
	}
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
				a.recordStatusChanged(ctx, inc.ID, string(db.StatusManualTestNeeded))
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
		if !a.VerifyResolved(ctx, itemID, inc.Title, preFix) {
			a.log.WarnContext(ctx, "post-fix verification failed, escalating", "incident", inc.ID)
			result.RequiresApproval = true
			result.EscalateAction = EscalateManualInvestigation
			result.PrimaryReason = "autonomous fix applied but playback verification failed; " + result.PrimaryReason
		}
	}

	if err := a.db.SetIncidentFinding(ctx, inc.ID, result, result); err != nil {
		a.log.ErrorContext(ctx, "set finding", "error", err)
	}
	if a.journal != nil {
		if err := a.journal.DiagnosisCompleted(ctx, inc.ID, result); err != nil {
			a.log.WarnContext(ctx, "record diagnosis_completed", "incident", inc.ID, "error", err)
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

// captureSignature snapshots a title's current observable state: what
// Jellyfin reports for the item right now, whether the path Jellyfin
// currently points at (which can change between runs — the underlying file
// may be replaced) is actually readable, and — best-effort — whether *arr
// actually has a file for this title at all (see arrHasFileForTitle).
// Every failure is recorded in the signature rather than returned as an
// error, since a snapshot must never abort a diagnosis. Returns a signature
// with only the *arr fields populated for an empty itemID (common on a
// freshly-reported incident whose Jellyfin item hasn't been resolved yet) —
// VerifyResolved treats a fully-zero signature as "no baseline", not as
// "unchanged".
func (a *Agent) captureSignature(ctx context.Context, itemID, title string) *FixSignature {
	sig := &FixSignature{}
	if a.disp != nil {
		sig.ArrHasFile, sig.ArrChecked = a.arrHasFileForTitle(ctx, title)
	}
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

// arrHasFileForTitle attempts to resolve whether Sonarr/Radarr has every
// file for title as a whole (every episode, for TV; the one file, for a
// movie) — best-effort, since an incident's media type isn't structurally
// known at this point (only the LLM parses that from title/details during
// diagnosis). Tries Sonarr first, then Radarr; returns checked=false if
// neither is configured or neither has a matching title, meaning "this
// signal isn't available" — VerifyResolved must never treat that the same
// as "confirmed missing".
//
// This is whole-title rather than single-episode granularity: the codebase
// has no season/episode parser outside the LLM's own reasoning, so a
// specific-episode check (as arr_search_missing's pending-outcome tracking
// does, where the season/episode came from the tool call that triggered it)
// isn't available here. Even at whole-title granularity this is a real
// improvement: resolveMediaStatus's whole-series HasFile is false if ANY
// episode is missing, which is a ground-truth signal Jellyfin's own episode
// index can't provide — Jellyfin kept reporting the Rick and Morty series as
// healthy (9 of 10 episodes indexed) for as long as S09E09 was missing.
// Returns (hasFile, checked).
func (a *Agent) arrHasFileForTitle(ctx context.Context, title string) (bool, bool) {
	if title == "" {
		return false, false
	}
	if a.disp.Sonarr != nil {
		if status, err := a.disp.resolveMediaStatus(ctx, client.ReplaceMediaTV, title, -1, -1); err == nil {
			return status.HasFile, true
		}
	}
	if a.disp.Radarr != nil {
		if status, err := a.disp.resolveMediaStatus(ctx, client.ReplaceMediaMovie, title, -1, -1); err == nil {
			return status.HasFile, true
		}
	}
	return false, false
}

// VerifyResolved reports whether a playback problem looks resolved for an
// item. Two independent requirements, both needed:
//
//  1. A hard gate: if *arr's file state could be confirmed and it says the
//     content is missing, nothing else matters — Jellyfin/dd signals can't
//     make missing content playable, and this is exactly the gap that let
//     the Rick and Morty S09E09 incident "verify" purely because the series'
//     other 9 episodes stayed indexed throughout.
//  2. Real usability evidence, and — when pre is non-nil — that post is a
//     STRICT IMPROVEMENT over it, not just different (see improved). An
//     identical-or-worse signature used to pass as long as it merely
//     differed from pre in any direction, including a lower episode count.
func (a *Agent) VerifyResolved(ctx context.Context, itemID, title string, pre *FixSignature) bool {
	post := a.captureSignature(ctx, itemID, title)

	if post.ArrChecked && !post.ArrHasFile {
		return false
	}

	sourceOK := post.SourceCount > 0 && post.Path != "" && post.DDBytesRead > 0 && post.DDError == ""
	episodesOK := post.EpisodeCount > 0
	if !sourceOK && !episodesOK {
		return false
	}
	if pre == nil {
		return true
	}
	return improved(pre, post)
}

// improved reports whether post is a genuine improvement over pre along at
// least one dimension VerifyResolved cares about, not merely different from
// it: newly readable when it wasn't, now resolving to a different path that
// itself reads successfully (the underlying file was replaced/relinked),
// more episodes indexed than before (not fewer, not the same), or *arr's
// file state newly confirmed present when it previously wasn't known or
// wasn't there.
func improved(pre, post *FixSignature) bool {
	preReadable := pre.DDBytesRead > 0 && pre.DDError == ""
	postReadable := post.DDBytesRead > 0 && post.DDError == ""
	switch {
	case postReadable && !preReadable:
		return true
	case postReadable && preReadable && post.Path != "" && post.Path != pre.Path:
		return true
	case post.EpisodeCount > pre.EpisodeCount:
		return true
	case post.ArrChecked && post.ArrHasFile && (!pre.ArrChecked || !pre.ArrHasFile):
		return true
	default:
		return false
	}
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

// PendingOutcomeObservation is what CheckPendingOutcome reports after polling
// live *arr state for one pending arr_search_missing outcome. Deliberately
// pure observation with no time-based decisions (grace periods, stall
// timeouts) — those live in Service.advancePendingOutcome, which has the
// outcome's history (StartedAt, LastProgressAt) to compare a fresh
// observation against.
type PendingOutcomeObservation struct {
	// HasFile is true once *arr actually has a file for the target — the
	// terminal "resolved" signal.
	HasFile bool
	// QueueStage is Sonarr/Radarr's queue status string ("queued",
	// "downloading", "completed", ...) for the matching queue item, or ""
	// if nothing is currently in the queue for this target (still waiting
	// on an indexer, no release exists yet, or it already imported and
	// dropped out of the queue — HasFile is what distinguishes the last case).
	QueueStage string
	// ProgressPct is 0-100, meaningful only when QueueStage != "".
	ProgressPct float64
}

// CheckPendingOutcome polls live Sonarr/Radarr state for one pending
// arr_search_missing outcome: whether the target now has a file, and — if
// not — whether anything is actively in the download queue for it. Matching
// a queue record to the target uses series/movie ID, which resolveMediaStatus
// already resolved from the title, rather than the more granular episode ID:
// good enough to answer "is anything happening for this show/movie", which
// is the question that matters for the season/series-scope searches this
// also has to support.
func (a *Agent) CheckPendingOutcome(ctx context.Context, po *db.PendingOutcome) (*PendingOutcomeObservation, error) {
	status, err := a.disp.resolveMediaStatus(ctx, po.MediaType, po.Title, po.Season, po.Episode)
	if err != nil {
		return nil, err
	}
	obs := &PendingOutcomeObservation{HasFile: status.HasFile}
	if obs.HasFile {
		return obs, nil
	}

	queue, queueErr := a.fetchQueue(ctx, po.MediaType)
	if queueErr != nil {
		// Best-effort: a transient queue-fetch failure shouldn't be mistaken
		// for "nothing is happening" (which would restart the grace-period
		// clock's reasoning in the caller) — report what we know (not yet
		// resolved) and let the next scheduled check try again.
		return obs, nil //nolint:nilerr // deliberate: see comment above, not a swallowed error
	}
	for _, q := range queue {
		if !queueRecordMatches(q, po.MediaType, status) {
			continue
		}
		obs.QueueStage = q.Status
		if q.Size > 0 {
			obs.ProgressPct = (q.Size - q.SizeLeft) / q.Size * progressPercentScale
		}
		break
	}
	return obs, nil
}

// progressPercentScale converts a fraction (Size-SizeLeft)/Size into a 0-100
// percentage for PendingOutcomeObservation.ProgressPct.
const progressPercentScale = 100.0

func (a *Agent) fetchQueue(ctx context.Context, mediaType string) ([]client.QueueRecord, error) {
	switch mediaType {
	case client.ReplaceMediaMovie:
		if a.disp.Radarr == nil {
			return nil, nil
		}
		return a.disp.Radarr.GetQueue(ctx)
	default:
		if a.disp.Sonarr == nil {
			return nil, nil
		}
		return a.disp.Sonarr.GetQueue(ctx)
	}
}

func queueRecordMatches(q client.QueueRecord, mediaType string, status *MediaStatusResult) bool {
	if mediaType == client.ReplaceMediaMovie {
		return q.MovieID == status.MovieID
	}
	return q.SeriesID == status.SeriesID
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
%s%s
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
		a.disruptionNote(ctx, inc.ID),
	)
}

// disruptionQuietWindow is how long after a service-wide disruptive action a
// newly-starting run is warned about it. Comfortably above waitUntilReady's
// restartReadyTimeout (30s) and a typical library-scan-trigger call, so the
// warning covers the period where the disrupted service is plausibly still
// settling, not indefinitely.
const disruptionQuietWindow = 2 * time.Minute

// disruptionNote returns a warning for a run starting shortly after a
// service-wide action (restart, scan, cleanup, sweep) was applied — possibly
// by a different, concurrently-running incident (see runManager.globalSlot,
// which prevents them literally overlapping, but not a fresh run starting
// just after one finishes while the disrupted service is still settling).
// Returns "" when nothing was recorded, the disruption is stale, or it was
// this same incident's own action (already covered by attemptHistory, and
// not something to warn an incident about itself).
func (a *Agent) disruptionNote(ctx context.Context, incidentID string) string {
	if a.journal == nil {
		return ""
	}
	disp, err := a.journal.LastDisruption(ctx)
	if err != nil || disp.IncidentID == incidentID || time.Since(disp.At) > disruptionQuietWindow {
		return ""
	}
	return fmt.Sprintf(
		"\nNOTE: %s was run %s ago while investigating incident #%s. Transient errors or missing "+
			"paths seen just now may be artifacts of that disruption, not this incident's own "+
			"problem — see the RESILIENCE note above.\n",
		disp.Action, time.Since(disp.At).Round(time.Second), shortID(disp.IncidentID),
	)
}

// shortID returns the first 8 characters of a UUID for compact display, or
// the whole string if it's shorter (defensive — every real incident ID is a
// full UUID, but a test fixture might not be).
func shortID(id string) string {
	const shortLen = 8
	if len(id) <= shortLen {
		return id
	}
	return id[:shortLen]
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

// identityParamKeys are the tool argument keys used across the registry to
// identify *what* an action targets, checked by identityParamsMatch.
func identityParamKeys() []string {
	return []string{paramItemID, paramName, paramTitle, paramSeriesID, paramMovieID, paramSeason, paramEpisode}
}

// identityParamsMatch reports whether a and b identify the same target: both
// empty (a parameterless action like restart_jellyfin), or agreeing on every
// identity key present in both. Checked individually instead of comparing
// full JSON objects because logAction sometimes records extra resolved
// fields the LLM's own call arguments never contain — e.g. sonarr_rescan
// logs series_id alongside the title the LLM passed.
//
// Every shared key must match, not just the first one found: with only the
// first checked, two arr_search_missing calls for different episodes of the
// same series (season/episode differ, but title — checked earlier in the
// key list — is identical) were wrongly treated as the same action and the
// second was blocked as a duplicate before ever running. season/episode
// must both be checked for a title match to mean anything for that tool.
func identityParamsMatch(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	matchedAny := false
	for _, k := range identityParamKeys() {
		av, aok := a[k]
		bv, bok := b[k]
		if !aok || !bok {
			continue
		}
		if fmt.Sprint(av) != fmt.Sprint(bv) {
			return false
		}
		matchedAny = true
	}
	return matchedAny
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

// logTurn is process-wide operational visibility (journalctl/Loki), separate
// from the durable per-incident transcript recordRound writes to the event
// log — the two serve different audiences and neither replaces the other.
// The "run that's still making progress" signal FindStaleInvestigating's
// sweeper needs no longer lives here: any event append (recordRound above,
// via a.journal) already updates the incident's most recent activity, so a
// dedicated heartbeat write is no longer necessary.
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

// BuildSummarySeed constructs the seed messages for a resumed run from a
// prior-session summary. The summary is explicitly labeled as unverified
// prior inference, not established fact, and the seed requires the FULL
// protocol to be re-run (not just two of five steps) before any conclusion.
//
// A resumed run re-diagnosing "100 meters" once cited ffprobe errors from a
// two-day-old summary as its root cause — the errors were actually about a
// completely different file, but the seed only mandated refreshing
// loki_query and get_disk_info, so nothing else was re-checked and the stale
// inference was never challenged. Requiring the full protocol, and requiring
// the eventual diagnosis to cite evidence gathered in THIS run rather than
// just repeating what the summary said, closes both halves of that gap.
func (a *Agent) BuildSummarySeed(ctx context.Context, inc *db.Incident, summary string) []openai.ChatCompletionMessage {
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
		{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf(
			"This is a resumed investigation of incident %q (type: %s).\n\n"+
				"Previous findings — UNVERIFIED prior inference from an earlier run, NOT established fact. "+
				"Conditions may have changed since, and even at the time it may have drawn a conclusion from "+
				"evidence about a different file, a transient disruption, or a stale log window:\n%s\n%s%s\n\n"+
				"Continue the diagnosis. Before calling complete_diagnosis you MUST re-run the FULL protocol "+
				"for this incident type from the system prompt above — for a playback problem, all five "+
				"steps (including dd_readability_test, and arr_media_status if content appears missing), not "+
				"just loki_query and get_disk_info; for an infrastructure problem, both steps. Do not skip a "+
				"step because the summary already contains similar-looking data — that data may be stale or "+
				"about the wrong file. Your complete_diagnosis.primary_reason MUST cite evidence YOU observed "+
				"in this run; a claim carried over from the previous findings above, on its own, is not "+
				"sufficient grounds for an action.",
			inc.Title,
			inc.What,
			summary,
			a.attemptHistory(ctx, inc.ID),
			a.disruptionNote(ctx, inc.ID),
		)},
	}
}
