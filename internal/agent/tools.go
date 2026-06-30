package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
)

// Tool name constants used in both toolDefs and dispatch.
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
	toolDecypharrRecheck     = "decypharr_recheck"
	toolRestartDecypharr     = "restart_decypharr"
	toolRestartJellyfin      = "restart_jellyfin"
	toolSonarrRescan         = "sonarr_rescan"
	toolRadarrRescan         = "radarr_rescan"
	toolClearJellyfinCache   = "clear_jellyfin_cache"
	toolListDirectory        = "list_directory"
	toolGetDiskInfo          = "get_disk_info"
	toolCompleteDiagnosis    = "complete_diagnosis"
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

const (
	paramTitle  = "title"
	paramItemID = "item_id"
	paramName   = "name"
)

const toolDefsCapacity = 19

// toolDefs returns the OpenAI function/tool definitions the agent can call.
func toolDefs() []openai.Tool {
	tools := make([]openai.Tool, 0, toolDefsCapacity)
	tools = append(tools, diagnosticToolDefs()...)
	tools = append(tools, actionToolDefs()...)
	tools = append(tools, completionToolDef())
	return tools
}

func diagnosticToolDefs() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: toolJellyfinSearch,
				Description: "Search Jellyfin for a media item by title. Returns up to 5 matches with item ID, type (Movie/Series/Episode), and file path. " +
					"For TV episodes, search by series name only — strip season/episode qualifiers before calling " +
					"(e.g. for 'The Boys S01E02' or 'the boys s1 episode 2', search 'The Boys'). " +
					"Use this first when the incident has no Jellyfin item ID.",
				Parameters: jsonSchema(map[string]any{
					paramTitle: param("string", "Show or movie name (TV: series name only, no S/E numbers)"),
				}, []string{paramTitle}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolJellyfinPlayback,
				Description: "Call Jellyfin PlaybackInfo for an item. Returns media sources, whether transcoding is needed, and any error codes. Empty sources mean Jellyfin cannot open the file.",
				Parameters: jsonSchema(map[string]any{
					paramItemID: param("string", "Jellyfin item ID"),
				}, []string{paramItemID}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: toolJellyfinListEpisodes,
				Description: "List the episodes Jellyfin has indexed for a Series item. An empty result means the " +
					"series exists but has no playable episodes indexed — the classic cause of an unplayable show. " +
					"Use this to confirm/verify episode indexing for a series item ID.",
				Parameters: jsonSchema(map[string]any{
					paramItemID: param("string", "Jellyfin Series item ID"),
				}, []string{paramItemID}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: toolJellyfinScanStatus,
				Description: "Check whether a Jellyfin library scan is currently running and its progress percentage. " +
					"Call this before jellyfin_library_scan so you never re-trigger a scan that is already in progress.",
				Parameters: jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolDDReadability,
				Description: "Run a non-destructive dd read test on a file path on the media host. Returns bytes read, speed, and any I/O error. EIO errors confirm a debrid/link problem.",
				Parameters: jsonSchema(map[string]any{
					"file_path": param("string", "Absolute path to the file on the media host FUSE mount"),
				}, []string{"file_path"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolGetTorrentState,
				Description: "List decypharr torrents matching a search term, returning their name, state, debrid provider, and download hash.",
				Parameters: jsonSchema(map[string]any{
					"search": param("string", "Search term (title or hash)"),
				}, []string{"search"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolLokiQuery,
				Description: "Query Loki for recent log lines from jellyfin or decypharr around the incident time. Returns up to 100 relevant lines.",
				Parameters: jsonSchema(map[string]any{
					"units":        param("string", `LogQL stream selector, e.g. {unit=~"jellyfin|decypharr"}`),
					"minutes_back": param("number", "How many minutes before now to search (max 120)"),
				}, []string{"units", "minutes_back"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolListDirectory,
				Description: "List the contents of a directory on the media host. Use this to find the actual video file inside a torrent folder before calling dd_readability_test. Only paths under /mnt/decypharr, /var/cache/decypharr, or /data are allowed.",
				Parameters: jsonSchema(map[string]any{
					"path": param("string", "Absolute directory path to list"),
				}, []string{"path"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolGetDiskInfo,
				Description: "Get disk usage for the media host mount points: /mnt/decypharr (FUSE media files), /var/cache/decypharr (decypharr cache), and /data. Use to check if a mount is present (non-zero total) and whether disk space is a factor.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
	}
}

func actionToolDefs() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolRefreshLinks,
				Description: "Trigger decypharr to re-unrestrict download URLs for broken entries (link refresh repair sweep). Use when dd shows EIO errors.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolRepairSweep,
				Description: "Trigger a general decypharr repair sweep without link refresh. Use after link refresh fails or to check for other broken entries.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: toolDecypharrRecheck,
				Description: "Recheck a single decypharr entry by name (and optionally apply a fix). More targeted than a " +
					"full repair sweep — use when get_torrent_state shows one specific broken/errored entry.",
				Parameters: jsonSchema(map[string]any{
					paramName: param("string", "Entry/torrent name as shown by get_torrent_state"),
					"fix":     param("boolean", "Whether to apply a fix (default true)"),
				}, []string{paramName}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name: toolJellyfinLibraryScan,
				Description: "Trigger a full Jellyfin library scan (rebuilds the index so on-disk items get picked up). " +
					"Non-destructive but server-wide and slow. Always call jellyfin_scan_status first; if a scan is " +
					"already running, do NOT call this — wait for it instead. Returns status started or already_running.",
				Parameters: jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolRestartDecypharr,
				Description: "Restart the decypharr service. Use when decypharr appears stuck or the repair sweep hangs.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolRestartJellyfin,
				Description: "Restart the Jellyfin service on the media host.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolSonarrRescan,
				Description: "Trigger Sonarr to rescan the disk for a series by title.",
				Parameters: jsonSchema(map[string]any{
					paramTitle: param("string", "Series title as known in Sonarr"),
				}, []string{paramTitle}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolRadarrRescan,
				Description: "Trigger Radarr to rescan the disk for a movie by title.",
				Parameters: jsonSchema(map[string]any{
					paramTitle: param("string", "Movie title as known in Radarr"),
				}, []string{paramTitle}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        toolClearJellyfinCache,
				Description: "Force a full metadata and image refresh for a Jellyfin item.",
				Parameters: jsonSchema(map[string]any{
					paramItemID: param("string", "Jellyfin item ID"),
				}, []string{paramItemID}),
			},
		},
	}
}

func completionToolDef() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        toolCompleteDiagnosis,
			Description: "Record the agent's diagnostic conclusion and ranked action recommendations, then end the diagnostic phase.",
			Parameters: jsonSchema(map[string]any{
				"root_cause":      param("string", "Concise description of the diagnosed root cause"),
				"confidence":      param("string", "high|medium|low"),
				"primary_action":  param("string", "The first action to take"),
				"primary_reason":  param("string", "Why this action addresses the root cause"),
				"fallback_action": param("string", "Action to take if primary fails (optional)"),
				"escalate_action": param("string", "Approval-required action if autonomous fixes fail (optional)"),
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
	}
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

	result, err := d.dispatch(ctx, name, args)
	if err != nil {
		return jsonResult(map[string]any{keyError: err.Error()})
	}
	return jsonResult(result)
}

func (d *Dispatcher) dispatch(ctx context.Context, name string, args map[string]any) (any, error) {
	if name == toolCompleteDiagnosis {
		return args, nil // handled by the agent loop directly
	}
	if isReadOnlyTool(name) {
		return d.dispatchRead(ctx, name, args)
	}
	return d.dispatchWrite(ctx, name, args)
}

func isReadOnlyTool(name string) bool {
	switch name {
	case toolJellyfinSearch, toolJellyfinPlayback, toolJellyfinListEpisodes,
		toolJellyfinScanStatus, toolDDReadability, toolGetTorrentState,
		toolLokiQuery, toolListDirectory, toolGetDiskInfo:
		return true
	}
	return false
}

func (d *Dispatcher) dispatchRead(ctx context.Context, name string, args map[string]any) (any, error) {
	switch name {
	case toolJellyfinSearch:
		title, _ := args[paramTitle].(string)
		return d.Jellyfin.SearchItem(ctx, title)

	case toolJellyfinPlayback:
		itemID, _ := args[paramItemID].(string)
		return d.Jellyfin.PlaybackInfo(ctx, itemID)

	case toolJellyfinListEpisodes:
		itemID, _ := args[paramItemID].(string)
		return d.Jellyfin.ListEpisodes(ctx, itemID)

	case toolJellyfinScanStatus:
		return d.Jellyfin.ScanStatus(ctx)

	case toolDDReadability:
		filePath, _ := args["file_path"].(string)
		if d.MediaAgent == nil {
			return nil, errMediaAgentNotConfigured
		}
		return d.MediaAgent.DDReadabilityTest(ctx, filePath)

	case toolGetTorrentState:
		search, _ := args["search"].(string)
		return d.Decypharr.ListTorrents(ctx, search, "")

	case toolLokiQuery:
		units, _ := args["units"].(string)
		minutes, _ := args["minutes_back"].(float64)
		if minutes <= 0 || minutes > maxLokiMinutes {
			minutes = defaultLokiMinutes
		}
		to := time.Now()
		from := to.Add(-time.Duration(minutes) * time.Minute)
		return d.Loki.QueryRange(ctx, units, from, to, lokiResultLimit)

	case toolListDirectory:
		path, _ := args["path"].(string)
		if d.MediaAgent == nil {
			return nil, errMediaAgentNotConfigured
		}
		return d.MediaAgent.ListDirectory(ctx, path)

	case toolGetDiskInfo:
		if d.MediaAgent == nil {
			return nil, errMediaAgentNotConfigured
		}
		return d.MediaAgent.DiskUsage(ctx)
	}

	return nil, errors.New("unknown read tool: " + name)
}

func (d *Dispatcher) dispatchWrite(ctx context.Context, name string, args map[string]any) (any, error) {
	switch name {
	case toolRefreshLinks:
		runID, err := d.Decypharr.RefreshLinks(ctx)
		if err != nil {
			return nil, err
		}
		d.logAction(ctx, toolRefreshLinks, nil)
		return map[string]string{keyRunID: runID, keyStatus: statusStarted}, nil

	case toolRepairSweep:
		runID, err := d.Decypharr.RunRepairSweep(ctx)
		if err != nil {
			return nil, err
		}
		d.logAction(ctx, "repair_sweep", nil)
		return map[string]string{keyRunID: runID, keyStatus: statusStarted}, nil

	case toolDecypharrRecheck:
		return d.dispatchDecypharrRecheck(ctx, args)

	case toolJellyfinLibraryScan:
		return d.dispatchLibraryScan(ctx)

	case toolRestartDecypharr:
		if err := d.Decypharr.Restart(ctx); err != nil {
			return nil, err
		}
		d.logAction(ctx, toolRestartDecypharr, nil)
		return map[string]string{keyStatus: "restarted"}, nil

	case toolRestartJellyfin:
		if d.MediaAgent == nil {
			return nil, errMediaAgentNotConfigured
		}
		if err := d.MediaAgent.RestartService(ctx, "jellyfin"); err != nil {
			return nil, err
		}
		d.logAction(ctx, toolRestartJellyfin, nil)
		return map[string]string{keyStatus: "restarted"}, nil

	case toolSonarrRescan:
		return d.dispatchSonarrRescan(ctx, args)

	case toolRadarrRescan:
		return d.dispatchRadarrRescan(ctx, args)

	case toolClearJellyfinCache:
		itemID, _ := args[paramItemID].(string)
		if err := d.Jellyfin.DeleteCache(ctx, itemID); err != nil {
			return nil, err
		}
		d.logAction(ctx, toolClearJellyfinCache, map[string]any{paramItemID: itemID})
		return map[string]string{keyStatus: "cache_cleared"}, nil
	}

	return nil, errors.New("unknown write tool: " + name)
}

// dispatchLibraryScan triggers a Jellyfin library scan, but first checks whether
// one is already running so the agent never re-triggers an in-progress scan.
func (d *Dispatcher) dispatchLibraryScan(ctx context.Context) (any, error) {
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

// logAction records a completed action to the database.
func (d *Dispatcher) logAction(ctx context.Context, action string, params map[string]any) {
	_ = d.DB.LogAction(ctx, &db.ActionLog{
		IncidentID:  d.IncidentID,
		Action:      action,
		Params:      params,
		TriggeredBy: triggeredByAgent,
		Status:      db.ActionApplied,
	})
}

// --- helpers ---

func jsonResult(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonSchema(props map[string]any, required []string) json.RawMessage {
	s := map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
	b, _ := json.Marshal(s)
	return b
}

func param(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}
