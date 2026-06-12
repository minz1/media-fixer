package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/db"
	openai "github.com/sashabaranov/go-openai"
)

// toolDefs returns the OpenAI function/tool definitions the agent can call.
func toolDefs() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "jellyfin_playback_info",
				Description: "Call Jellyfin PlaybackInfo for an item. Returns media sources, whether transcoding is needed, and any error codes. Empty sources mean Jellyfin cannot open the file.",
				Parameters: jsonSchema(map[string]any{
					"item_id": param("string", "Jellyfin item ID"),
				}, []string{"item_id"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "dd_readability_test",
				Description: "Run a non-destructive dd read test on a file path on the media host. Returns bytes read, speed, and any I/O error. EIO errors confirm a debrid/link problem.",
				Parameters: jsonSchema(map[string]any{
					"file_path": param("string", "Absolute path to the file on the media host FUSE mount"),
				}, []string{"file_path"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "get_torrent_state",
				Description: "List decypharr torrents matching a search term, returning their name, state, debrid provider, and download hash.",
				Parameters: jsonSchema(map[string]any{
					"search": param("string", "Search term (title or hash)"),
				}, []string{"search"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "loki_query",
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
				Name:        "refresh_decypharr_links",
				Description: "Trigger decypharr to re-unrestrict download URLs for broken entries (link refresh repair sweep). Use when dd shows EIO errors.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "decypharr_repair_sweep",
				Description: "Trigger a general decypharr repair sweep without link refresh. Use after link refresh fails or to check for other broken entries.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "restart_decypharr",
				Description: "Restart the decypharr service. Use when decypharr appears stuck or the repair sweep hangs.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "restart_jellyfin",
				Description: "Restart the Jellyfin service on the media host.",
				Parameters:  jsonSchema(map[string]any{}, []string{}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "sonarr_rescan",
				Description: "Trigger Sonarr to rescan the disk for a series by title.",
				Parameters: jsonSchema(map[string]any{
					"title": param("string", "Series title as known in Sonarr"),
				}, []string{"title"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "radarr_rescan",
				Description: "Trigger Radarr to rescan the disk for a movie by title.",
				Parameters: jsonSchema(map[string]any{
					"title": param("string", "Movie title as known in Radarr"),
				}, []string{"title"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "clear_jellyfin_cache",
				Description: "Force a full metadata and image refresh for a Jellyfin item.",
				Parameters: jsonSchema(map[string]any{
					"item_id": param("string", "Jellyfin item ID"),
				}, []string{"item_id"}),
			},
		},
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "complete_diagnosis",
				Description: "Record the agent's diagnostic conclusion and ranked action recommendations, then end the diagnostic phase.",
				Parameters: jsonSchema(map[string]any{
					"root_cause":  param("string", "Concise description of the diagnosed root cause"),
					"confidence":  param("string", "high|medium|low"),
					"primary_action": param("string", "The first action to take"),
					"primary_reason": param("string", "Why this action addresses the root cause"),
					"fallback_action": param("string", "Action to take if primary fails (optional)"),
					"escalate_action": param("string", "Approval-required action if autonomous fixes fail (optional)"),
					"requires_approval": param("boolean", "Whether any recommended action requires owner approval"),
				}, []string{"root_cause", "confidence", "primary_action", "primary_reason"}),
			},
		},
	}
}

// Dispatcher holds the clients needed to execute tool calls.
type Dispatcher struct {
	Decypharr *client.DecypharrClient
	Jellyfin  *client.JellyfinClient
	Sonarr    *client.ArrClient
	Radarr    *client.ArrClient
	Loki      *client.LokiClient
	Media     *client.MediaHostClient
	DB        *db.DB
	IncidentID string
}

// Dispatch executes a tool call and returns a JSON string result for the LLM.
func (d *Dispatcher) Dispatch(ctx context.Context, name string, argsJSON string) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(argsJSON), &args)

	result, err := d.dispatch(ctx, name, args)
	if err != nil {
		return jsonResult(map[string]any{"error": err.Error()})
	}
	return jsonResult(result)
}

func (d *Dispatcher) dispatch(ctx context.Context, name string, args map[string]any) (any, error) {
	switch name {
	case "jellyfin_playback_info":
		itemID, _ := args["item_id"].(string)
		return d.Jellyfin.PlaybackInfo(ctx, itemID)

	case "dd_readability_test":
		filePath, _ := args["file_path"].(string)
		if d.Media == nil {
			return nil, fmt.Errorf("media host client not configured")
		}
		return d.Media.DDReadabilityTest(ctx, filePath)

	case "get_torrent_state":
		search, _ := args["search"].(string)
		return d.Decypharr.ListTorrents(ctx, search, "")

	case "loki_query":
		units, _ := args["units"].(string)
		minutes, _ := args["minutes_back"].(float64)
		if minutes <= 0 || minutes > 120 {
			minutes = 30
		}
		to := time.Now()
		from := to.Add(-time.Duration(minutes) * time.Minute)
		return d.Loki.QueryRange(ctx, units, from, to, 100)

	case "refresh_decypharr_links":
		runID, err := d.Decypharr.RefreshLinks(ctx)
		if err != nil {
			return nil, err
		}
		_ = d.DB.LogAction(ctx, &db.ActionLog{
			IncidentID:  d.IncidentID,
			Action:      "refresh_links",
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
		return map[string]string{"run_id": runID, "status": "started"}, nil

	case "decypharr_repair_sweep":
		runID, err := d.Decypharr.RunRepairSweep(ctx)
		if err != nil {
			return nil, err
		}
		_ = d.DB.LogAction(ctx, &db.ActionLog{
			IncidentID:  d.IncidentID,
			Action:      "repair_sweep",
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
		return map[string]string{"run_id": runID, "status": "started"}, nil

	case "restart_decypharr":
		if err := d.Decypharr.Restart(ctx); err != nil {
			return nil, err
		}
		_ = d.DB.LogAction(ctx, &db.ActionLog{
			IncidentID:  d.IncidentID,
			Action:      "restart_decypharr",
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
		return map[string]string{"status": "restarted"}, nil

	case "restart_jellyfin":
		if d.Media == nil {
			return nil, fmt.Errorf("media host client not configured")
		}
		if err := d.Media.RestartService(ctx, "jellyfin"); err != nil {
			return nil, err
		}
		_ = d.DB.LogAction(ctx, &db.ActionLog{
			IncidentID:  d.IncidentID,
			Action:      "restart_jellyfin",
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
		return map[string]string{"status": "restarted"}, nil

	case "sonarr_rescan":
		title, _ := args["title"].(string)
		series, err := d.Sonarr.SearchSeries(ctx, title)
		if err != nil {
			return nil, err
		}
		if series == nil {
			return map[string]string{"error": "series not found in sonarr"}, nil
		}
		if err := d.Sonarr.RescanSeries(ctx, series.ID); err != nil {
			return nil, err
		}
		_ = d.DB.LogAction(ctx, &db.ActionLog{
			IncidentID:  d.IncidentID,
			Action:      "sonarr_rescan",
			Params:      map[string]any{"series_id": series.ID, "title": title},
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
		return map[string]any{"series_id": series.ID, "status": "rescan_queued"}, nil

	case "radarr_rescan":
		title, _ := args["title"].(string)
		movie, err := d.Radarr.SearchMovie(ctx, title)
		if err != nil {
			return nil, err
		}
		if movie == nil {
			return map[string]string{"error": "movie not found in radarr"}, nil
		}
		if err := d.Radarr.RescanMovie(ctx, movie.ID); err != nil {
			return nil, err
		}
		_ = d.DB.LogAction(ctx, &db.ActionLog{
			IncidentID:  d.IncidentID,
			Action:      "radarr_rescan",
			Params:      map[string]any{"movie_id": movie.ID, "title": title},
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
		return map[string]any{"movie_id": movie.ID, "status": "rescan_queued"}, nil

	case "clear_jellyfin_cache":
		itemID, _ := args["item_id"].(string)
		if err := d.Jellyfin.DeleteCache(ctx, itemID); err != nil {
			return nil, err
		}
		_ = d.DB.LogAction(ctx, &db.ActionLog{
			IncidentID:  d.IncidentID,
			Action:      "clear_jellyfin_cache",
			Params:      map[string]any{"item_id": itemID},
			TriggeredBy: "agent",
			Status:      db.ActionApplied,
		})
		return map[string]string{"status": "cache_cleared"}, nil

	case "complete_diagnosis":
		return args, nil // handled by the agent loop directly

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// --- helpers ---

func jsonResult(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonSchema(props map[string]any, required []string) json.RawMessage {
	schema := map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
	b, _ := json.Marshal(schema)
	return b
}

func param(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func strArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}
