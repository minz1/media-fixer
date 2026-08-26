package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

// Tool name constants used in both the registry and the system prompt.
const (
	toolJellyfinSearch       = "jellyfin_search"
	toolJellyfinPlayback     = "jellyfin_playback_info"
	toolJellyfinListEpisodes = "jellyfin_list_episodes"
	toolJellyfinScanStatus   = "jellyfin_scan_status"
	toolJellyfinLibraryScan  = "jellyfin_library_scan"
	toolDDReadability        = "dd_readability_test"
	toolGetTorrentState      = "get_torrent_state"
	toolLokiQuery            = "loki_query"
	toolRefreshLinks         = "refresh_decypharr_links"
	toolRepairSweep          = "decypharr_repair_sweep"
	toolRepairStatus         = "get_repair_status"
	toolRepairHealth         = "get_repair_health"
	toolCacheCleanup         = "decypharr_cache_cleanup"
	toolDecypharrRecheck     = "decypharr_recheck"
	toolRestartDecypharr     = "restart_decypharr"
	toolRestartJellyfin      = "restart_jellyfin"
	toolSonarrRescan         = "sonarr_rescan"
	toolRadarrRescan         = "radarr_rescan"
	toolClearJellyfinCache   = "clear_jellyfin_cache"
	toolListDirectory        = "list_directory"
	toolGetDiskInfo          = "get_disk_info"
	toolCompleteDiagnosis    = "complete_diagnosis"
	toolArrRemoveAndSearch   = "arr_remove_and_search"
)

// Shared map key names for JSON results.
const (
	keyStatus = "status"
	keyError  = "error"
	keyRunID  = "run_id"
)

// statusStarted is the result value for an action that kicked off async work.
const statusStarted = "started"

// triggeredByAgent is the value stored in action log records for agent-initiated actions.
const triggeredByAgent = "agent"

// Loki query limits.
const (
	maxLokiMinutes     = 120
	defaultLokiMinutes = 30.0
	lokiResultLimit    = 100
)

var errMediaAgentNotConfigured = errors.New("media-agent not configured")

// diskInfoDesc documents the two independent per-path signals the tool returns.
const diskInfoDesc = "Get disk usage for the media host paths: /mnt/decypharr (FUSE media files), " +
	"/var/cache/decypharr (cache), and /data. Each entry has two independent booleans plus byte counts. " +
	"accessible = os.Stat succeeds (path reachable). is_mount_point = a filesystem is actually mounted there. " +
	"For /mnt/decypharr, is_mount_point=false means the FUSE mount died and fell back to an empty directory — " +
	"it looks healthy (accessible=true, non-zero bytes) but is down. total_bytes=0 with both booleans true is " +
	"normal (cloud-backed). For /data and /var/cache/decypharr, is_mount_point=false is normal."

const (
	paramTitle  = "title"
	paramItemID = "item_id"
	paramName   = "name"
)

// Escalation action names the agent may set in complete_diagnosis.escalate_action.
// These are the only owner-approval-required actions the agent may recommend;
// each is executed via the dashboard's approve-escalation flow, never autonomously.
const (
	EscalateNone                = "none"
	EscalateRemoveAndSearch     = "remove_and_search"
	EscalateManualInvestigation = "manual_investigation"
)

// escalateActionEnum lists every value the escalate_action schema accepts.
func escalateActionEnum() []string {
	return []string{EscalateNone, EscalateRemoveAndSearch, EscalateManualInvestigation}
}

// Field names inside complete_diagnosis.escalate_params, used when
// escalate_action is remove_and_search.
const (
	paramMediaType = "media_type"
	paramScope     = "scope"
	paramSeason    = "season"
	paramEpisode   = "episode"
	paramBlocklist = "blocklist"
)

// toolRisk classifies what invoking a tool does to the system, and therefore
// how the agent loop and any live-check tooling are allowed to treat it.
type toolRisk int

const (
	// riskRead tools have no side effects: free to call, never counts as an action.
	riskRead toolRisk = iota
	// riskWrite tools are autonomous actions: they count against maxAutonomousActions.
	riskWrite
	// riskApproval tools require owner approval and are never offered to the LLM
	// as a callable tool — they exist in the registry so an approval workflow
	// (and live-check tooling) can dispatch them by name.
	riskApproval
	// riskControl is complete_diagnosis, handled specially by the agent loop.
	riskControl
)

// toolSpec is one entry in the tool registry: its LLM-facing definition, its
// risk class, and the dispatcher method that executes it.
type toolSpec struct {
	Name    string
	Def     *openai.FunctionDefinition
	Risk    toolRisk
	Handler func(*Dispatcher, context.Context, map[string]any) (any, error)
}

const toolRegistryCapacity = 22

// toolRegistry is the single source of truth for every tool the agent knows
// about: its schema, its risk class, and how to execute it. toolDefs (what the
// LLM sees), Dispatch/Call (how a call is executed), and isAutonomousAction
// all derive from this table instead of maintaining parallel lists that can
// drift out of sync.
func toolRegistry() []toolSpec {
	specs := make([]toolSpec, 0, toolRegistryCapacity)
	specs = append(specs, readToolSpecs()...)
	specs = append(specs, repairReadToolSpecs()...)
	specs = append(specs, actionToolSpecs()...)
	specs = append(specs, approvalToolSpecs()...)
	specs = append(specs, completionSpec())
	return specs
}

// readToolSpecs returns the Jellyfin and host-diagnostic read-only tools.
func readToolSpecs() []toolSpec {
	specs := jellyfinReadToolSpecs()
	specs = append(specs, hostReadToolSpecs()...)
	return specs
}

// jellyfinReadToolSpecs returns the Jellyfin read-only diagnostic tools.
func jellyfinReadToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolJellyfinSearch,
			Def: &openai.FunctionDefinition{
				Name: toolJellyfinSearch,
				Description: "Search Jellyfin for a media item by title. Returns up to 5 matches with item ID, type (Movie/Series/Episode), and file path. " +
					"For TV episodes, search by series name only — strip season/episode qualifiers before calling " +
					"(e.g. for 'The Boys S01E02' or 'the boys s1 episode 2', search 'The Boys'). " +
					"Use this first when the incident has no Jellyfin item ID.",
				Parameters: jsonSchema(map[string]any{
					paramTitle: param("string", "Show or movie name (TV: series name only, no S/E numbers)"),
				}, []string{paramTitle}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readJellyfinSearch,
		},
		{
			Name: toolJellyfinPlayback,
			Def: &openai.FunctionDefinition{
				Name:        toolJellyfinPlayback,
				Description: "Call Jellyfin PlaybackInfo for an item. Returns media sources, whether transcoding is needed, and any error codes. Empty sources mean Jellyfin cannot open the file.",
				Parameters: jsonSchema(map[string]any{
					paramItemID: param("string", "Jellyfin item ID"),
				}, []string{paramItemID}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readJellyfinPlayback,
		},
		{
			Name: toolJellyfinListEpisodes,
			Def: &openai.FunctionDefinition{
				Name: toolJellyfinListEpisodes,
				Description: "List the episodes Jellyfin has indexed for a Series item. An empty result means the " +
					"series exists but has no playable episodes indexed — the classic cause of an unplayable show. " +
					"Use this to confirm/verify episode indexing for a series item ID.",
				Parameters: jsonSchema(map[string]any{
					paramItemID: param("string", "Jellyfin Series item ID"),
				}, []string{paramItemID}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readJellyfinListEpisodes,
		},
		{
			Name: toolJellyfinScanStatus,
			Def: &openai.FunctionDefinition{
				Name: toolJellyfinScanStatus,
				Description: "Check whether a Jellyfin library scan is currently running and its progress percentage. " +
					"Call this before jellyfin_library_scan so you never re-trigger a scan that is already in progress.",
				Parameters: jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readJellyfinScanStatus,
		},
	}
}

// hostReadToolSpecs returns the media-host read-only diagnostic tools (dd
// test, torrent state, logs, directory listing, disk info).
func hostReadToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolDDReadability,
			Def: &openai.FunctionDefinition{
				Name:        toolDDReadability,
				Description: "Run a non-destructive dd read test on a file path on the media host. Returns bytes read, speed, and any I/O error. EIO errors confirm a debrid/link problem.",
				Parameters: jsonSchema(map[string]any{
					"file_path": param("string", "Absolute path to the file on the media host FUSE mount"),
				}, []string{"file_path"}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readDDReadability,
		},
		{
			Name: toolGetTorrentState,
			Def: &openai.FunctionDefinition{
				Name:        toolGetTorrentState,
				Description: "List decypharr torrents matching a search term, returning their name, state, debrid provider, and download hash.",
				Parameters: jsonSchema(map[string]any{
					"search": param("string", "Search term (title or hash)"),
				}, []string{"search"}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readTorrentState,
		},
		{
			Name: toolLokiQuery,
			Def: &openai.FunctionDefinition{
				Name:        toolLokiQuery,
				Description: "Query Loki for recent log lines from jellyfin or decypharr around the incident time. Returns up to 100 relevant lines.",
				Parameters: jsonSchema(map[string]any{
					"units":        param("string", lokiUnitParamDesc),
					"minutes_back": param("number", "How many minutes before now to search (max 120)"),
				}, []string{"units", "minutes_back"}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readLokiQuery,
		},
		{
			Name: toolListDirectory,
			Def: &openai.FunctionDefinition{
				Name:        toolListDirectory,
				Description: "List the contents of a directory on the media host. Use this to find the actual video file inside a torrent folder before calling dd_readability_test. Only paths under /mnt/decypharr, /var/cache/decypharr, or /data are allowed.",
				Parameters: jsonSchema(map[string]any{
					"path": param("string", "Absolute directory path to list"),
				}, []string{"path"}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readListDirectory,
		},
		{
			Name: toolGetDiskInfo,
			Def: &openai.FunctionDefinition{
				Name:        toolGetDiskInfo,
				Description: diskInfoDesc,
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readDiskInfo,
		},
	}
}

// repairReadToolSpecs returns the read-only decypharr repair diagnostics that let
// the agent inspect repair state without spending an autonomous action.
func repairReadToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolRepairStatus,
			Def: &openai.FunctionDefinition{
				Name: toolRepairStatus,
				Description: "Check whether a decypharr repair job is currently running. Call this before " +
					"refresh_decypharr_links or decypharr_repair_sweep so you never stack a second repair on " +
					"top of one already in progress.",
				Parameters: jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readRepairStatus,
		},
		{
			Name: toolRepairHealth,
			Def: &openai.FunctionDefinition{
				Name: toolRepairHealth,
				Description: "List decypharr entry health records (read-only). Use this to identify which " +
					"specific entries are broken so you can target decypharr_recheck by name instead of running " +
					"a full repair sweep.",
				Parameters: jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskRead,
			Handler: (*Dispatcher).readRepairHealth,
		},
	}
}

// actionToolSpecs returns the autonomous (write) action tools.
func actionToolSpecs() []toolSpec {
	specs := decypharrActionToolSpecs()
	specs = append(specs, jellyfinAndArrActionToolSpecs()...)
	return specs
}

// decypharrActionToolSpecs returns the decypharr-side autonomous actions.
func decypharrActionToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolRefreshLinks,
			Def: &openai.FunctionDefinition{
				Name:        toolRefreshLinks,
				Description: "Trigger decypharr to re-unrestrict download URLs for broken entries (link refresh repair sweep). Use when dd shows EIO errors.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchRefreshLinks,
		},
		{
			Name: toolRepairSweep,
			Def: &openai.FunctionDefinition{
				Name:        toolRepairSweep,
				Description: "Trigger a general decypharr repair sweep without link refresh. Use after link refresh fails or to check for other broken entries.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchRepairSweep,
		},
		{
			Name: toolCacheCleanup,
			Def: &openai.FunctionDefinition{
				Name: toolCacheCleanup,
				Description: "Run a decypharr FUSE mount cache cleanup cycle. Use when the mount serves stale " +
					"paths (EIO through the mount but the underlying debrid link is fine) — a lighter fix than " +
					"restart_decypharr.",
				Parameters: jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchCacheCleanup,
		},
		{
			Name: toolDecypharrRecheck,
			Def: &openai.FunctionDefinition{
				Name: toolDecypharrRecheck,
				Description: "Recheck a single decypharr entry by name (and optionally apply a fix). More targeted than a " +
					"full repair sweep — use when get_torrent_state shows one specific broken/errored entry.",
				Parameters: jsonSchema(map[string]any{
					paramName: param("string", "Entry/torrent name as shown by get_torrent_state"),
					"fix":     param("boolean", "Whether to apply a fix (default true)"),
				}, []string{paramName}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchDecypharrRecheck,
		},
	}
}

// jellyfinAndArrActionToolSpecs returns the Jellyfin, Sonarr, and Radarr
// autonomous actions.
func jellyfinAndArrActionToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolJellyfinLibraryScan,
			Def: &openai.FunctionDefinition{
				Name: toolJellyfinLibraryScan,
				Description: "Trigger a full Jellyfin library scan (rebuilds the index so on-disk items get picked up). " +
					"Non-destructive but server-wide and slow. Always call jellyfin_scan_status first; if a scan is " +
					"already running, do NOT call this — wait for it instead. Returns status started or already_running.",
				Parameters: jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchLibraryScan,
		},
		{
			Name: toolRestartDecypharr,
			Def: &openai.FunctionDefinition{
				Name:        toolRestartDecypharr,
				Description: "Restart the decypharr service. Use when decypharr appears stuck or the repair sweep hangs.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchRestartDecypharr,
		},
		{
			Name: toolRestartJellyfin,
			Def: &openai.FunctionDefinition{
				Name:        toolRestartJellyfin,
				Description: "Restart the Jellyfin service on the media host.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchRestartJellyfin,
		},
		{
			Name: toolSonarrRescan,
			Def: &openai.FunctionDefinition{
				Name:        toolSonarrRescan,
				Description: "Trigger Sonarr to rescan the disk for a series by title.",
				Parameters: jsonSchema(map[string]any{
					paramTitle: param("string", "Series title as known in Sonarr"),
				}, []string{paramTitle}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchSonarrRescan,
		},
		{
			Name: toolRadarrRescan,
			Def: &openai.FunctionDefinition{
				Name:        toolRadarrRescan,
				Description: "Trigger Radarr to rescan the disk for a movie by title.",
				Parameters: jsonSchema(map[string]any{
					paramTitle: param("string", "Movie title as known in Radarr"),
				}, []string{paramTitle}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchRadarrRescan,
		},
		{
			Name: toolClearJellyfinCache,
			Def: &openai.FunctionDefinition{
				Name:        toolClearJellyfinCache,
				Description: "Force a full metadata and image refresh for a Jellyfin item.",
				Parameters: jsonSchema(map[string]any{
					paramItemID: param("string", "Jellyfin item ID"),
				}, []string{paramItemID}),
			},
			Risk:    riskWrite,
			Handler: (*Dispatcher).dispatchClearJellyfinCache,
		},
	}
}

// approvalToolSpecs returns tools that require owner approval and are never
// offered to the LLM as a callable tool (see toolDefs). The handler here is a
// read-only PlanReplace preview — never a delete — which is what makes it
// safe for live-check tooling to exercise unconditionally. Actually deleting
// files and searching only happens via Agent.RunEscalation, invoked by the
// dashboard's approve-escalation flow after an owner reviews this same plan.
func approvalToolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name: toolArrRemoveAndSearch,
			Def: &openai.FunctionDefinition{
				Name: toolArrRemoveAndSearch,
				Description: "Preview a Sonarr/Radarr remove-and-re-search: which file(s) would be deleted, " +
					"which grabs would be blocklisted, and what search would follow. Owner-approval-only — " +
					"never called by the agent directly; recommend it via complete_diagnosis.escalate_action " +
					"instead.",
				Parameters: jsonSchema(arrRemoveAndSearchProps(), []string{paramMediaType, paramTitle}),
			},
			Risk:    riskApproval,
			Handler: (*Dispatcher).readArrRemoveAndSearchPlan,
		},
	}
}

// arrRemoveAndSearchProps is the parameter schema shared by the
// arr_remove_and_search tool and complete_diagnosis's escalate_params.
func arrRemoveAndSearchProps() map[string]any {
	return map[string]any{
		paramMediaType: param("string", "tv|movie"),
		paramTitle:     param("string", "Series or movie title"),
		paramScope:     param("string", "episode|season|series (tv only; ignored for movie)"),
		paramSeason:    param("integer", "Season number (tv, required when scope is episode or season)"),
		paramEpisode:   param("integer", "Episode number (tv, required when scope is episode)"),
		paramBlocklist: param(
			"boolean",
			"Whether to blocklist the bad release so it isn't grabbed again (default true)",
		),
	}
}

func completionSpec() toolSpec {
	return toolSpec{
		Name: toolCompleteDiagnosis,
		Def: &openai.FunctionDefinition{
			Name:        toolCompleteDiagnosis,
			Description: "Record the agent's diagnostic conclusion and ranked action recommendations, then end the diagnostic phase.",
			Parameters: jsonSchema(map[string]any{
				"root_cause":      param("string", "Concise description of the diagnosed root cause"),
				"confidence":      param("string", "high|medium|low"),
				"primary_action":  param("string", "The first action to take"),
				"primary_reason":  param("string", "Why this action addresses the root cause"),
				"fallback_action": param("string", "Action to take if primary fails (optional)"),
				"escalate_action": enumParam(
					"string",
					"Owner-approval-required action if autonomous fixes fail (optional). "+
						"remove_and_search deletes the bad file(s), blocklists the release, and re-searches — "+
						"set escalate_params when using it.",
					escalateActionEnum(),
				),
				"escalate_params": objectParam(
					"Parameters for escalate_action; required when escalate_action is remove_and_search.",
					arrRemoveAndSearchProps(),
				),
				"requires_approval": param(
					"boolean",
					"Whether any recommended action requires owner approval",
				),
				"verify_after_seconds": param(
					"integer",
					"If you applied a non-destructive fix that needs time (e.g. a library scan or "+
						"refresh), set this to your best estimate in seconds before the fix should be "+
						"verified. The system will re-check up to 5 times instead of escalating. Set 0/omit "+
						"if no verification is needed.",
				),
				"verify_item_id": param(
					"string",
					"Jellyfin item ID to re-check during verification (optional; defaults to the incident item)",
				),
				"user_eta_minutes": param(
					"integer",
					"Friendly estimate in minutes for when the reporter should try again, used in the "+
						"\"should be fixed soon\" message (optional)",
				),
			}, []string{"root_cause", "confidence", "primary_action", "primary_reason"}),
		},
		Risk:    riskControl,
		Handler: (*Dispatcher).completeDiagnosis,
	}
}

// toolDefs returns the OpenAI function/tool definitions the agent can call.
// Approval-required tools are excluded — they are dispatchable by name (see
// Dispatcher.Call) but only ever invoked via an explicit owner-approval flow,
// never offered to the LLM directly.
func toolDefs() []openai.Tool {
	reg := toolRegistry()
	tools := make([]openai.Tool, 0, len(reg))
	for _, spec := range reg {
		if spec.Risk == riskApproval {
			continue
		}
		tools = append(tools, openai.Tool{Type: openai.ToolTypeFunction, Function: spec.Def})
	}
	return tools
}

// ToolNames returns every tool name in the registry, including
// approval-gated ones. Used by live-check tooling to verify coverage.
func ToolNames() []string {
	reg := toolRegistry()
	names := make([]string, len(reg))
	for i, spec := range reg {
		names[i] = spec.Name
	}
	return names
}

// ExcludedFromLiveCheck reports whether a tool has no external system to
// exercise live — currently just complete_diagnosis, which the agent loop
// handles internally rather than dispatching to any client. Live-check
// tooling uses this to scope its "every tool has a check" coverage
// requirement to tools that actually call out to a service.
func ExcludedFromLiveCheck(name string) bool {
	spec, ok := specByName(name)
	return ok && spec.Risk == riskControl
}

// specByName looks up a tool's registry entry by name.
func specByName(name string) (toolSpec, bool) {
	for _, spec := range toolRegistry() {
		if spec.Name == name {
			return spec, true
		}
	}
	return toolSpec{}, false
}

func isAutonomousAction(toolName string) bool {
	spec, ok := specByName(toolName)
	return ok && spec.Risk == riskWrite
}

// Dispatcher holds the clients needed to execute tool calls.
type Dispatcher struct {
	Decypharr  *client.DecypharrClient
	Jellyfin   *client.JellyfinClient
	Sonarr     *client.ArrClient
	Radarr     *client.ArrClient
	Loki       *client.LokiClient
	MediaAgent *client.MediaAgentClient
	DB         *db.DB
	IncidentID string
}

// Dispatch executes a tool call and returns a JSON string result for the LLM.
func (d *Dispatcher) Dispatch(ctx context.Context, name string, argsJSON string) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &args)

	result, err := d.Call(ctx, name, args)
	if err != nil {
		return jsonResult(map[string]any{keyError: err.Error()})
	}
	return jsonResult(result)
}

// Call executes a tool by name with already-decoded arguments and returns the
// typed result. Unlike Dispatch, errors are returned rather than folded into
// the JSON payload — this is what live-check tooling uses to tell "tool
// errored" apart from "tool returned an error-shaped result".
func (d *Dispatcher) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	spec, ok := specByName(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	return spec.Handler(d, ctx, args)
}

// completeDiagnosis is handled by the agent loop directly; the dispatcher just
// hands the arguments back unchanged so it can share the same Call path.
func (d *Dispatcher) completeDiagnosis(_ context.Context, args map[string]any) (any, error) {
	return args, nil
}

// --- read handlers ---

func (d *Dispatcher) readJellyfinSearch(ctx context.Context, args map[string]any) (any, error) {
	title, _ := args[paramTitle].(string)
	return d.Jellyfin.SearchItem(ctx, title)
}

func (d *Dispatcher) readJellyfinPlayback(ctx context.Context, args map[string]any) (any, error) {
	itemID, _ := args[paramItemID].(string)
	return d.Jellyfin.PlaybackInfo(ctx, itemID)
}

func (d *Dispatcher) readJellyfinListEpisodes(ctx context.Context, args map[string]any) (any, error) {
	itemID, _ := args[paramItemID].(string)
	return d.Jellyfin.ListEpisodes(ctx, itemID)
}

func (d *Dispatcher) readJellyfinScanStatus(ctx context.Context, _ map[string]any) (any, error) {
	return d.Jellyfin.ScanStatus(ctx)
}

func (d *Dispatcher) readDDReadability(ctx context.Context, args map[string]any) (any, error) {
	filePath, _ := args["file_path"].(string)
	if d.MediaAgent == nil {
		return nil, errMediaAgentNotConfigured
	}
	return d.MediaAgent.DDReadabilityTest(ctx, filePath)
}

func (d *Dispatcher) readTorrentState(ctx context.Context, args map[string]any) (any, error) {
	search, _ := args["search"].(string)
	return d.Decypharr.ListTorrents(ctx, search, "")
}

func (d *Dispatcher) readRepairStatus(ctx context.Context, _ map[string]any) (any, error) {
	return d.Decypharr.RepairStatus(ctx)
}

func (d *Dispatcher) readRepairHealth(ctx context.Context, _ map[string]any) (any, error) {
	return d.Decypharr.RepairHealth(ctx)
}

func (d *Dispatcher) readLokiQuery(ctx context.Context, args map[string]any) (any, error) {
	units, _ := args["units"].(string)
	units = FixLokiUnitSelector(units)
	minutes, _ := args["minutes_back"].(float64)
	if minutes <= 0 || minutes > maxLokiMinutes {
		minutes = defaultLokiMinutes
	}
	to := time.Now()
	from := to.Add(-time.Duration(minutes) * time.Minute)
	return d.Loki.QueryRange(ctx, units, from, to, lokiResultLimit)
}

func (d *Dispatcher) readListDirectory(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["path"].(string)
	if d.MediaAgent == nil {
		return nil, errMediaAgentNotConfigured
	}
	return d.MediaAgent.ListDirectory(ctx, path)
}

func (d *Dispatcher) readDiskInfo(ctx context.Context, _ map[string]any) (any, error) {
	if d.MediaAgent == nil {
		return nil, errMediaAgentNotConfigured
	}
	return d.MediaAgent.DiskUsage(ctx)
}

// --- write (autonomous action) handlers ---

func (d *Dispatcher) dispatchRefreshLinks(ctx context.Context, _ map[string]any) (any, error) {
	runID, err := d.Decypharr.RefreshLinks(ctx)
	if err != nil {
		return nil, err
	}
	d.logAction(ctx, toolRefreshLinks, nil)
	return map[string]string{keyRunID: runID, keyStatus: statusStarted}, nil
}

func (d *Dispatcher) dispatchRepairSweep(ctx context.Context, _ map[string]any) (any, error) {
	runID, err := d.Decypharr.RunRepairSweep(ctx)
	if err != nil {
		return nil, err
	}
	d.logAction(ctx, "repair_sweep", nil)
	return map[string]string{keyRunID: runID, keyStatus: statusStarted}, nil
}

func (d *Dispatcher) dispatchCacheCleanup(ctx context.Context, _ map[string]any) (any, error) {
	if err := d.Decypharr.MountCacheCleanup(ctx); err != nil {
		return nil, err
	}
	d.logAction(ctx, toolCacheCleanup, nil)
	return map[string]string{keyStatus: statusStarted}, nil
}

// dispatchLibraryScan triggers a Jellyfin library scan, but first checks whether
// one is already running so the agent never re-triggers an in-progress scan.
func (d *Dispatcher) dispatchLibraryScan(ctx context.Context, _ map[string]any) (any, error) {
	status, err := d.Jellyfin.ScanStatus(ctx)
	if err == nil && status.Running {
		return map[string]any{
			keyStatus:      "already_running",
			"progress_pct": status.ProgressPct,
			"note":         "a library scan is already in progress — do not trigger another; wait for it to finish",
		}, nil
	}
	if scanErr := d.Jellyfin.LibraryScan(ctx); scanErr != nil {
		return nil, scanErr
	}
	d.logAction(ctx, toolJellyfinLibraryScan, nil)
	return map[string]string{keyStatus: statusStarted}, nil
}

func (d *Dispatcher) dispatchRestartDecypharr(ctx context.Context, _ map[string]any) (any, error) {
	if d.MediaAgent == nil {
		return nil, errMediaAgentNotConfigured
	}
	if err := d.MediaAgent.RestartService(ctx, "decypharr"); err != nil {
		return nil, err
	}
	d.logAction(ctx, toolRestartDecypharr, nil)
	return map[string]string{keyStatus: "restarted"}, nil
}

func (d *Dispatcher) dispatchRestartJellyfin(ctx context.Context, _ map[string]any) (any, error) {
	if d.MediaAgent == nil {
		return nil, errMediaAgentNotConfigured
	}
	if err := d.MediaAgent.RestartService(ctx, "jellyfin"); err != nil {
		return nil, err
	}
	d.logAction(ctx, toolRestartJellyfin, nil)
	return map[string]string{keyStatus: "restarted"}, nil
}

// dispatchDecypharrRecheck rechecks a single named decypharr entry.
func (d *Dispatcher) dispatchDecypharrRecheck(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args[paramName].(string)
	if name == "" {
		return map[string]string{keyError: "name is required"}, nil
	}
	fix := true
	if v, ok := args["fix"].(bool); ok {
		fix = v
	}
	if err := d.Decypharr.RecheckEntry(ctx, name, fix); err != nil {
		return nil, err
	}
	d.logAction(ctx, toolDecypharrRecheck, map[string]any{paramName: name, "fix": fix})
	return map[string]string{keyStatus: "rechecked"}, nil
}

func (d *Dispatcher) dispatchSonarrRescan(ctx context.Context, args map[string]any) (any, error) {
	title, _ := args[paramTitle].(string)
	series, err := d.Sonarr.SearchSeries(ctx, title)
	if errors.Is(err, client.ErrNotFound) {
		return map[string]string{keyError: "series not found in sonarr"}, nil
	}
	if err != nil {
		return nil, err
	}
	if rescanErr := d.Sonarr.RescanSeries(ctx, series.ID); rescanErr != nil {
		return nil, rescanErr
	}
	d.logAction(ctx, toolSonarrRescan, map[string]any{"series_id": series.ID, paramTitle: title})
	return map[string]any{"series_id": series.ID, keyStatus: "rescan_queued"}, nil
}

func (d *Dispatcher) dispatchRadarrRescan(ctx context.Context, args map[string]any) (any, error) {
	title, _ := args[paramTitle].(string)
	movie, err := d.Radarr.SearchMovie(ctx, title)
	if errors.Is(err, client.ErrNotFound) {
		return map[string]string{keyError: "movie not found in radarr"}, nil
	}
	if err != nil {
		return nil, err
	}
	if rescanErr := d.Radarr.RescanMovie(ctx, movie.ID); rescanErr != nil {
		return nil, rescanErr
	}
	d.logAction(ctx, toolRadarrRescan, map[string]any{"movie_id": movie.ID, paramTitle: title})
	return map[string]any{"movie_id": movie.ID, keyStatus: "rescan_queued"}, nil
}

func (d *Dispatcher) dispatchClearJellyfinCache(ctx context.Context, args map[string]any) (any, error) {
	itemID, _ := args[paramItemID].(string)
	if err := d.Jellyfin.DeleteCache(ctx, itemID); err != nil {
		return nil, err
	}
	d.logAction(ctx, toolClearJellyfinCache, map[string]any{paramItemID: itemID})
	return map[string]string{keyStatus: "cache_cleared"}, nil
}

// --- approval (owner-approved escalation) handlers ---

// readArrRemoveAndSearchPlan builds a read-only preview of a Sonarr/Radarr
// remove-and-re-search — never deletes anything. This is the tool's Handler,
// so it is what a live-check (and the dashboard's Preview button) both call.
func (d *Dispatcher) readArrRemoveAndSearchPlan(ctx context.Context, args map[string]any) (any, error) {
	req, arrClient, err := d.buildReplaceRequest(args)
	if err != nil {
		return nil, err
	}
	return arrClient.PlanReplace(ctx, req)
}

// executeArrRemoveAndSearch re-resolves and then executes a remove-and-search
// (blocklist grabs, delete files, trigger a search). It is never reachable
// from Dispatcher.Call under the tool's own name — only Agent.RunEscalation,
// invoked after owner approval, calls it directly.
func (d *Dispatcher) executeArrRemoveAndSearch(ctx context.Context, args map[string]any) (any, error) {
	req, arrClient, err := d.buildReplaceRequest(args)
	if err != nil {
		return nil, err
	}
	plan, err := arrClient.PlanReplace(ctx, req)
	if err != nil {
		return nil, err
	}
	result, err := arrClient.ExecuteReplace(ctx, plan)
	if err != nil {
		return nil, err
	}
	d.logAction(ctx, toolArrRemoveAndSearch, args)
	return result, nil
}

// buildReplaceRequest translates escalate_params-shaped args into a
// client.ReplaceRequest and picks the Sonarr or Radarr client to run it
// against based on media_type.
func (d *Dispatcher) buildReplaceRequest(args map[string]any) (client.ReplaceRequest, *client.ArrClient, error) {
	mediaType, _ := args[paramMediaType].(string)
	title, _ := args[paramTitle].(string)
	scope, _ := args[paramScope].(string)

	req := client.ReplaceRequest{
		MediaType: mediaType,
		Title:     title,
		Scope:     scope,
		Season:    intArgOrDefault(args, paramSeason, -1),
		Episode:   intArgOrDefault(args, paramEpisode, -1),
	}
	if blocklist, ok := args[paramBlocklist].(bool); ok {
		req.SkipBlocklist = !blocklist
	}

	switch mediaType {
	case client.ReplaceMediaTV:
		return req, d.Sonarr, nil
	case client.ReplaceMediaMovie:
		return req, d.Radarr, nil
	default:
		return req, nil, fmt.Errorf("arr_remove_and_search: unknown media_type %q", mediaType)
	}
}

// intArgOrDefault extracts an int from a JSON-decoded args map, where numbers
// always decode as float64. Returns def if the key is absent or not a number.
func intArgOrDefault(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}

// logAction records a completed action to the database. A nil DB (e.g. when
// the dispatcher is used by the live-check CLI, which has no incident to
// attach actions to) is a silent no-op rather than a panic.
func (d *Dispatcher) logAction(ctx context.Context, action string, params map[string]any) {
	if d.DB == nil {
		return
	}
	_ = d.DB.LogAction(ctx, &db.ActionLog{
		IncidentID:  d.IncidentID,
		Action:      action,
		Params:      params,
		TriggeredBy: triggeredByAgent,
		Status:      db.ActionApplied,
	})
}

// --- helpers ---

// unitSelectorRe matches the quoted value of a LogQL unit label selector,
// capturing the operator+opening-quote, the value, and the closing quote.
var unitSelectorRe = regexp.MustCompile(`(unit=~?")([^"]*)(")`)

// lokiUnitParamDesc is the tool parameter description for the `units` field.
// Kept as a constant to stay within the 120-char line limit at the call site.
const lokiUnitParamDesc = `LogQL stream selector, e.g. {unit=~"jellyfin.service|decypharr.service"}. ` +
	`Loki unit labels always carry the .service suffix; bare names like "jellyfin" will never match.`

// FixLokiUnitSelector ensures every unit name in a LogQL stream selector
// carries the ".service" suffix required by systemd-journal labels in Loki.
// LogQL =~ is a fully-anchored RE2 match, so {unit=~"jellyfin"} never
// matches an entry labelled "jellyfin.service".
func FixLokiUnitSelector(selector string) string {
	return unitSelectorRe.ReplaceAllStringFunc(selector, func(m string) string {
		sub := unitSelectorRe.FindStringSubmatch(m)
		const wantGroups = 4
		if len(sub) != wantGroups {
			return m
		}
		op, val := sub[1], sub[2]
		isRegex := strings.Contains(op, "~")
		parts := strings.Split(val, "|")
		for i, p := range parts {
			bare := strings.Trim(p, "()")
			if strings.HasSuffix(bare, ".service") || strings.HasSuffix(bare, `\.service`) {
				continue
			}
			if isRegex {
				parts[i] = bare + `\.service`
			} else {
				parts[i] = bare + ".service"
			}
		}
		return op + strings.Join(parts, "|") + `"`
	})
}

func jsonResult(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// jsonSchemaTypeKey is the JSON Schema "type" key, shared by jsonSchema,
// param, and objectParam. jsonSchemaTypeObject is its value wherever the
// schema node itself is an object (the root schema and objectParam).
const (
	jsonSchemaTypeKey    = "type"
	jsonSchemaTypeObject = "object"
)

func jsonSchema(props map[string]any, required []string) json.RawMessage {
	s := map[string]any{
		jsonSchemaTypeKey: jsonSchemaTypeObject,
		"properties":      props,
		"required":        required,
	}
	b, _ := json.Marshal(s)
	return b
}

func param(typ, desc string) map[string]any {
	return map[string]any{jsonSchemaTypeKey: typ, "description": desc}
}

// enumParam is param with a fixed set of accepted values.
func enumParam(typ, desc string, enum []string) map[string]any {
	p := param(typ, desc)
	p["enum"] = enum
	return p
}

// objectParam describes an optional nested object parameter with its own properties.
func objectParam(desc string, props map[string]any) map[string]any {
	return map[string]any{jsonSchemaTypeKey: jsonSchemaTypeObject, "description": desc, "properties": props}
}
