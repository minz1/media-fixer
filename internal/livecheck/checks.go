package livecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

// tier classifies when a check is allowed to run.
type tier string

const (
	tierRead       tier = "read"
	tierWrite      tier = "write"
	tierDisruptive tier = "disrupt"
	// tierDryRun checks are read-only previews of an otherwise destructive
	// action (arr_remove_and_search's PlanReplace) — always safe to run.
	tierDryRun tier = "approval-preview"
)

// Tool argument keys, matching the param names in internal/agent/tools.go.
const (
	argTitle       = "title"
	argItemID      = "item_id"
	argFilePath    = "file_path"
	argPath        = "path"
	argSearch      = "search"
	argName        = "name"
	argFix         = "fix"
	argUnits       = "units"
	argMinutesBack = "minutes_back"
	argMediaType   = "media_type"
	argScope       = "scope"
)

// defaultLokiMinutesBack is how far back the loki_query check looks.
const defaultLokiMinutesBack = 30

// checkFunc exercises one tool against live fixtures and reports the
// outcome. Tool/Risk/LatencyMS on the returned Result are filled in by the
// Runner; a checkFunc only sets Status/Detail/Error.
type checkFunc func(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, opts Options) Result

type checkSpec struct {
	Tool string
	Tier tier
	Run  checkFunc
}

// checkRegistry lists every check the live-check suite knows how to run.
// coverage_test.go verifies this covers every non-control tool in
// agent.ToolNames() — adding a tool to the agent's registry without adding a
// check here fails the build.
func checkRegistry() []checkSpec {
	specs := readCheckSpecs()
	specs = append(specs, writeCheckSpecs()...)
	specs = append(specs, disruptiveCheckSpecs()...)
	specs = append(specs, approvalCheckSpecs()...)
	return specs
}

func readCheckSpecs() []checkSpec {
	return []checkSpec{
		{Tool: "jellyfin_search", Tier: tierRead, Run: checkJellyfinSearch},
		{Tool: "jellyfin_playback_info", Tier: tierRead, Run: checkJellyfinPlayback},
		{Tool: "jellyfin_list_episodes", Tier: tierRead, Run: checkJellyfinListEpisodes},
		{Tool: "jellyfin_scan_status", Tier: tierRead, Run: checkJellyfinScanStatus},
		{Tool: "dd_readability_test", Tier: tierRead, Run: checkDDReadability},
		{Tool: "get_torrent_state", Tier: tierRead, Run: checkGetTorrentState},
		{Tool: "loki_query", Tier: tierRead, Run: checkLokiQuery},
		{Tool: "list_directory", Tier: tierRead, Run: checkListDirectory},
		{Tool: "get_disk_info", Tier: tierRead, Run: checkGetDiskInfo},
		{Tool: "get_repair_status", Tier: tierRead, Run: checkRepairStatus},
		{Tool: "get_repair_health", Tier: tierRead, Run: checkRepairHealth},
	}
}

func writeCheckSpecs() []checkSpec {
	return []checkSpec{
		{Tool: "refresh_decypharr_links", Tier: tierWrite, Run: checkRefreshLinks},
		{Tool: "decypharr_repair_sweep", Tier: tierWrite, Run: checkRepairSweep},
		{Tool: "decypharr_cache_cleanup", Tier: tierWrite, Run: checkCacheCleanup},
		{Tool: "decypharr_recheck", Tier: tierWrite, Run: checkDecypharrRecheck},
		{Tool: "sonarr_rescan", Tier: tierWrite, Run: checkSonarrRescan},
		{Tool: "radarr_rescan", Tier: tierWrite, Run: checkRadarrRescan},
		{Tool: "clear_jellyfin_cache", Tier: tierWrite, Run: checkClearJellyfinCache},
	}
}

func disruptiveCheckSpecs() []checkSpec {
	return []checkSpec{
		{Tool: "restart_decypharr", Tier: tierDisruptive, Run: checkRestartDecypharr},
		{Tool: "restart_jellyfin", Tier: tierDisruptive, Run: checkRestartJellyfin},
		{Tool: "jellyfin_library_scan", Tier: tierDisruptive, Run: checkJellyfinLibraryScan},
	}
}

func approvalCheckSpecs() []checkSpec {
	return []checkSpec{
		{Tool: "arr_remove_and_search", Tier: tierDryRun, Run: checkArrRemoveAndSearch},
	}
}

// --- read checks ---

func checkJellyfinSearch(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.SeriesTitle == "" {
		return degraded("no fixture: no series title discovered")
	}
	result, err := disp.Call(ctx, "jellyfin_search", map[string]any{argTitle: fx.SeriesTitle})
	return classify(result, err)
}

func checkJellyfinPlayback(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.JellyfinItemID == "" {
		return degraded("no fixture: no Jellyfin item discovered")
	}
	result, err := disp.Call(ctx, "jellyfin_playback_info", map[string]any{argItemID: fx.JellyfinItemID})
	return classify(result, err)
}

func checkJellyfinListEpisodes(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.JellyfinItemID == "" {
		return degraded("no fixture: no Jellyfin item discovered")
	}
	result, err := disp.Call(ctx, "jellyfin_list_episodes", map[string]any{argItemID: fx.JellyfinItemID})
	return classify(result, err)
}

func checkJellyfinScanStatus(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	result, err := disp.Call(ctx, "jellyfin_scan_status", map[string]any{})
	return classify(result, err)
}

func checkDDReadability(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if disp.MediaAgent == nil {
		return unconfiguredMediaAgent()
	}
	if fx.SamplePath == "" {
		return degraded("no fixture: no sample file discovered")
	}
	result, err := disp.Call(ctx, "dd_readability_test", map[string]any{argFilePath: fx.SamplePath})
	return classify(result, err)
}

func checkGetTorrentState(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	search := fx.TorrentName
	result, err := disp.Call(ctx, "get_torrent_state", map[string]any{argSearch: search})
	r := classifyDecypharr(result, err)
	if r.Status == StatusOK && search != "" && isEmptySlice(result) {
		r.Status = StatusDegraded
		r.Detail = "query ok, but the known-good torrent fixture wasn't found"
	}
	return r
}

func checkLokiQuery(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	args := map[string]any{
		argUnits:       `{unit=~"jellyfin.service|decypharr.service"}`,
		argMinutesBack: float64(defaultLokiMinutesBack),
	}
	result, err := disp.Call(ctx, "loki_query", args)
	r := classify(result, err)
	if r.Status != StatusOK {
		return r
	}
	if qr, ok := result.(*client.LokiQueryResult); ok && len(qr.Lines) == 0 {
		r.Status = StatusDegraded
		r.Detail = "query ok, 0 lines in the last 30 minutes — check the unit selector or Loki retention"
	}
	return r
}

func checkListDirectory(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if disp.MediaAgent == nil {
		return unconfiguredMediaAgent()
	}
	if fx.TorrentName == "" {
		return degraded("no fixture: no torrent discovered")
	}
	result, err := disp.Call(ctx, "list_directory", map[string]any{argPath: "/mnt/decypharr/" + fx.TorrentName})
	return classify(result, err)
}

func checkGetDiskInfo(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	if disp.MediaAgent == nil {
		return unconfiguredMediaAgent()
	}
	result, err := disp.Call(ctx, "get_disk_info", map[string]any{})
	return classify(result, err)
}

func checkRepairStatus(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	result, err := disp.Call(ctx, "get_repair_status", map[string]any{})
	return classifyDecypharr(result, err)
}

func checkRepairHealth(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	result, err := disp.Call(ctx, "get_repair_health", map[string]any{})
	return classifyDecypharr(result, err)
}

// --- write checks ---

func checkRefreshLinks(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	if skip, r := skipIfRepairRunning(ctx, disp); skip {
		return r
	}
	result, err := disp.Call(ctx, "refresh_decypharr_links", map[string]any{})
	return classifyDecypharr(result, err)
}

func checkRepairSweep(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	if skip, r := skipIfRepairRunning(ctx, disp); skip {
		return r
	}
	result, err := disp.Call(ctx, "decypharr_repair_sweep", map[string]any{})
	return classifyDecypharr(result, err)
}

func checkCacheCleanup(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	result, err := disp.Call(ctx, "decypharr_cache_cleanup", map[string]any{})
	return classifyDecypharr(result, err)
}

func checkDecypharrRecheck(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.RepairEntryName == "" {
		return degraded("no fixture: no repair entry name discovered")
	}
	args := map[string]any{argName: fx.RepairEntryName, argFix: false}
	result, err := disp.Call(ctx, "decypharr_recheck", args)
	return classifyDecypharr(result, err)
}

func checkSonarrRescan(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.SeriesTitle == "" {
		return degraded("no fixture: no series title discovered")
	}
	result, err := disp.Call(ctx, "sonarr_rescan", map[string]any{argTitle: fx.SeriesTitle})
	return classify(result, err)
}

func checkRadarrRescan(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.MovieTitle == "" {
		return degraded("no fixture: no movie title discovered")
	}
	result, err := disp.Call(ctx, "radarr_rescan", map[string]any{argTitle: fx.MovieTitle})
	return classify(result, err)
}

func checkClearJellyfinCache(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.JellyfinItemID == "" {
		return degraded("no fixture: no Jellyfin item discovered")
	}
	result, err := disp.Call(ctx, "clear_jellyfin_cache", map[string]any{argItemID: fx.JellyfinItemID})
	return classify(result, err)
}

// --- disruptive checks ---

func checkRestartDecypharr(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	if disp.MediaAgent == nil {
		return unconfiguredMediaAgent()
	}
	result, err := disp.Call(ctx, "restart_decypharr", map[string]any{})
	return classify(result, err)
}

func checkRestartJellyfin(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	if disp.MediaAgent == nil {
		return unconfiguredMediaAgent()
	}
	result, err := disp.Call(ctx, "restart_jellyfin", map[string]any{})
	return classify(result, err)
}

func checkJellyfinLibraryScan(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	// dispatchLibraryScan already self-checks scan status and reports
	// already_running instead of erroring, so no separate guard is needed
	// here — either outcome proves the tool works end to end.
	result, err := disp.Call(ctx, "jellyfin_library_scan", map[string]any{})
	return classify(result, err)
}

// --- approval (dry-run) checks ---

func checkArrRemoveAndSearch(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	args, reason, ok := arrRemoveAndSearchArgs(fx)
	if !ok {
		return degraded("no fixture: " + reason)
	}
	// This calls the tool's Handler directly, which is a read-only
	// PlanReplace preview — it never deletes anything, so it is always safe
	// to run regardless of -write/-disruptive.
	result, err := disp.Call(ctx, "arr_remove_and_search", args)
	return classify(result, err)
}

// arrRemoveAndSearchArgs prefers a TV fixture (more scope options to
// exercise) and falls back to a movie fixture.
func arrRemoveAndSearchArgs(fx *Fixtures) (map[string]any, string, bool) {
	switch {
	case fx.SeriesTitle != "":
		return map[string]any{
			argMediaType: "tv",
			argTitle:     fx.SeriesTitle,
			argScope:     "series",
		}, "", true
	case fx.MovieTitle != "":
		return map[string]any{
			argMediaType: "movie",
			argTitle:     fx.MovieTitle,
		}, "", true
	default:
		return nil, "no series or movie title discovered", false
	}
}

// --- shared helpers ---

// skipIfRepairRunning consults get_repair_status before a decypharr repair
// action, so the live-check suite never stacks a second repair sweep on top
// of one already in progress (the same rule the systemPrompt gives the
// agent, enforced here for real since the write tools don't self-check it).
func skipIfRepairRunning(ctx context.Context, disp *agent.Dispatcher) (bool, Result) {
	raw, err := disp.Decypharr.RepairStatus(ctx)
	if err != nil {
		// Can't determine status — proceed rather than block the check
		// entirely; the action call below will surface any real problem.
		return false, Result{}
	}
	var status struct {
		Running bool `json:"running"`
	}
	if jsonErr := json.Unmarshal(raw, &status); jsonErr == nil && status.Running {
		return true, Result{Status: StatusSkipped, Detail: "a decypharr repair is already running"}
	}
	return false, Result{}
}

func degraded(detail string) Result { return Result{Status: StatusDegraded, Detail: detail} }
func unconfiguredMediaAgent() Result {
	return Result{Status: StatusUnconfigured, Detail: "media_agent not configured"}
}

// classify is the default error→Status mapping: any error is a failure.
func classify(result any, err error) Result {
	if err != nil {
		return Result{Status: StatusFail, Error: err.Error()}
	}
	return Result{Status: StatusOK, Detail: summarize(result)}
}

// classifyDecypharr additionally maps client.ErrNotFound to StatusMissing —
// only decypharr's client surfaces ErrNotFound for a genuine HTTP 404,
// meaning the endpoint isn't present on this build/branch of decypharr,
// distinct from every other client's ErrNotFound (which mean "no match",
// a normal empty-result case handled by the caller, not a build mismatch).
func classifyDecypharr(result any, err error) Result {
	if errors.Is(err, client.ErrNotFound) {
		return Result{Status: StatusMissing, Error: "404 — endpoint not present on this decypharr build"}
	}
	return classify(result, err)
}

// summarize renders a best-effort human-readable one-liner for a tool
// result: slice length when possible, otherwise truncated JSON.
func summarize(v any) string {
	if v == nil {
		return ""
	}
	if n, ok := sliceLen(v); ok {
		return fmt.Sprintf("%d item(s)", n)
	}
	const maxDetailLen = 200
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	if len(b) > maxDetailLen {
		return string(b[:maxDetailLen]) + "…"
	}
	return string(b)
}

func sliceLen(v any) (int, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice {
		return rv.Len(), true
	}
	return 0, false
}

func isEmptySlice(v any) bool {
	n, ok := sliceLen(v)
	return ok && n == 0
}
