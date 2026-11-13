package livecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/client"
)

// Tool argument keys, matching the param names in internal/agent/tools.go.
const (
	argTitle       = "title"
	argLanguage    = "language"
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
	argReason      = "reason"
)

// defaultLokiMinutesBack is how far back the loki_query check looks.
const defaultLokiMinutesBack = 30

// checkFunc exercises one tool against live fixtures and reports the
// outcome. Tool/Risk/LatencyMS on the returned Result are filled in by the
// Runner; a checkFunc only sets Status/Detail/Error.
type checkFunc func(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, opts Options) Result

type checkSpec struct {
	Tool string
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
		{Tool: "jellyfin_search", Run: checkJellyfinSearch},
		{Tool: "jellyfin_playback_info", Run: checkJellyfinPlayback},
		{Tool: "jellyfin_list_episodes", Run: checkJellyfinListEpisodes},
		{Tool: "jellyfin_scan_status", Run: checkJellyfinScanStatus},
		{Tool: "dd_readability_test", Run: checkDDReadability},
		{Tool: "probe_streams", Run: checkProbeStreams},
		{Tool: "stream_alignment", Run: checkStreamAlignment},
		{Tool: "list_alternatives", Run: checkListAlternatives},
		{Tool: "sonarr_swap_plan", Run: checkSonarrSwapPlan},
		{Tool: "subtitle_extract_test", Run: checkSubtitleExtractTest},
		{Tool: "get_torrent_state", Run: checkGetTorrentState},
		{Tool: "loki_query", Run: checkLokiQuery},
		{Tool: "list_directory", Run: checkListDirectory},
		{Tool: "get_disk_info", Run: checkGetDiskInfo},
		{Tool: "get_repair_status", Run: checkRepairStatus},
		{Tool: "get_repair_health", Run: checkRepairHealth},
		{Tool: "arr_media_status", Run: checkArrMediaStatus},
		{Tool: "arr_grab_history", Run: checkArrGrabHistory},
	}
}

func writeCheckSpecs() []checkSpec {
	return []checkSpec{
		{Tool: "refresh_decypharr_links", Run: checkRefreshLinks},
		{Tool: "decypharr_repair_sweep", Run: checkRepairSweep},
		{Tool: "decypharr_cache_cleanup", Run: checkCacheCleanup},
		{Tool: "decypharr_recheck", Run: checkDecypharrRecheck},
		{Tool: "sonarr_rescan", Run: checkSonarrRescan},
		{Tool: "radarr_rescan", Run: checkRadarrRescan},
		{Tool: "clear_jellyfin_cache", Run: checkClearJellyfinCache},
		{Tool: "arr_search_missing", Run: checkArrSearchMissing},
		{Tool: "bazarr_search_subtitles", Run: checkBazarrSearch},
	}
}

func disruptiveCheckSpecs() []checkSpec {
	return []checkSpec{
		{Tool: "restart_decypharr", Run: checkRestartDecypharr},
		{Tool: "restart_jellyfin", Run: checkRestartJellyfin},
		{Tool: "jellyfin_library_scan", Run: checkJellyfinLibraryScan},
	}
}

func approvalCheckSpecs() []checkSpec {
	return []checkSpec{
		{Tool: "arr_remove_and_search", Run: checkArrRemoveAndSearch},
		{Tool: "decypharr_delete_readd", Run: checkDecypharrDeleteReadd},
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
	if fx.JellyfinPlaybackItemID == "" {
		return degraded("no fixture: no playback-safe Jellyfin item discovered")
	}
	result, err := disp.Call(ctx, "jellyfin_playback_info", map[string]any{argItemID: fx.JellyfinPlaybackItemID})
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
	if fx.SamplePath == "" {
		return degraded("no fixture: no sample file discovered")
	}
	result, err := disp.Call(ctx, "dd_readability_test", map[string]any{argFilePath: fx.SamplePath})
	return classify(result, err)
}

func checkProbeStreams(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.SamplePath == "" {
		return degraded("no fixture: no sample file discovered")
	}
	result, err := disp.Call(ctx, "probe_streams", map[string]any{argFilePath: fx.SamplePath})
	return classify(result, err)
}

func checkStreamAlignment(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.JellyfinPlaybackItemID == "" {
		return degraded("no fixture: no playback-safe Jellyfin item discovered")
	}
	result, err := disp.Call(ctx, "stream_alignment",
		map[string]any{argItemID: fx.JellyfinPlaybackItemID, argLanguage: "en"})
	return classify(result, err)
}

func checkSubtitleExtractTest(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.JellyfinPlaybackItemID == "" {
		return degraded("no fixture: no playback-safe Jellyfin item discovered")
	}
	raw, err := disp.Call(ctx, "jellyfin_playback_info", map[string]any{argItemID: fx.JellyfinPlaybackItemID})
	info, ok := raw.(*client.PlaybackInfoResult)
	if err != nil || !ok || len(info.MediaSources) == 0 {
		return classify(raw, err)
	}
	for _, s := range info.MediaSources[0].MediaStreams {
		if s.Type == "Subtitle" {
			extract, extractErr := disp.Call(ctx, "subtitle_extract_test",
				map[string]any{argItemID: fx.JellyfinPlaybackItemID, "stream_index": s.Index})
			if errors.Is(extractErr, agent.ErrSubtitleFileTooLarge) {
				return degraded("no fixture: the playback item is too large to extract an embedded subtitle")
			}
			return classify(extract, extractErr)
		}
	}
	return degraded("no fixture: the playback item has no subtitle stream")
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
	if qr, ok := result.(*agent.LokiQueryOutput); ok && len(qr.Lines) == 0 {
		r.Status = StatusDegraded
		r.Detail = "query ok, 0 lines in the last 30 minutes — check the unit selector or Loki retention"
	}
	return r
}

// checkListDirectory tries every directory layout this stack has used for a
// torrent's files (see decypharrCandidateDirs) rather than assuming
// /mnt/decypharr/<torrent> directly — real files often live under
// /mnt/decypharr/__all__/<torrent>/ instead, and a hardcoded single guess
// here previously ENOENT'd even when discoverSamplePath (which already tries
// both) had found the file fine.
func checkListDirectory(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.TorrentName == "" {
		return degraded("no fixture: no torrent discovered")
	}
	if fx.TorrentIsFile {
		result, err := disp.Call(ctx, "list_directory", map[string]any{argPath: decypharrAllDir})
		return classify(result, err)
	}
	var lastErr error
	for _, dir := range decypharrCandidateDirs(fx.TorrentFolder, fx.TorrentName) {
		result, err := disp.Call(ctx, "list_directory", map[string]any{argPath: dir})
		if err == nil {
			return classify(result, nil)
		}
		lastErr = err
	}
	return classify(nil, lastErr)
}

func checkGetDiskInfo(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
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

// arrMediaStatusArgs prefers a TV fixture (exercises the season/episode
// breakdown) and falls back to a movie fixture, matching
// arrRemoveAndSearchArgs's preference below.
func arrMediaStatusArgs(fx *Fixtures) (map[string]any, string, bool) {
	switch {
	case fx.SeriesTitle != "":
		return map[string]any{argMediaType: "tv", argTitle: fx.SeriesTitle}, "", true
	case fx.MovieTitle != "":
		return map[string]any{argMediaType: "movie", argTitle: fx.MovieTitle}, "", true
	default:
		return nil, "no series or movie title discovered", false
	}
}

func checkArrMediaStatus(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	args, reason, ok := arrMediaStatusArgs(fx)
	if !ok {
		return degraded("no fixture: " + reason)
	}
	result, err := disp.Call(ctx, "arr_media_status", args)
	return classify(result, err)
}

func checkArrGrabHistory(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	args, reason, ok := arrMediaStatusArgs(fx)
	if !ok {
		return degraded("no fixture: " + reason)
	}
	result, err := disp.Call(ctx, "arr_grab_history", args)
	return classify(result, err)
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

// checkArrSearchMissing exercises the tool's own safety precondition rather
// than a real search: the discovered fixture title is drawn from the live
// library, so on a healthy setup it already has a file, and the tool is
// expected to refuse with agent.ErrArrTargetHasFile — that refusal IS the
// pass condition, proving the guard actually re-resolves and checks current
// state rather than trusting its caller. A real, on-disk-missing title would
// make this genuinely trigger a search, which is an acceptable (and correct)
// outcome too — either way the tool worked as designed.
func checkArrSearchMissing(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	args, reason, ok := arrMediaStatusArgs(fx)
	if !ok {
		return degraded("no fixture: " + reason)
	}
	result, err := disp.Call(ctx, "arr_search_missing", args)
	if errors.Is(err, agent.ErrArrTargetHasFile) {
		return Result{Status: StatusOK, Detail: "refused as expected: target already has a file"}
	}
	return classify(result, err)
}

func checkBazarrSearch(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	_, err := disp.Call(ctx, "bazarr_search_subtitles", map[string]any{
		argMediaType: "tv", argTitle: "livecheck", argLanguage: "zz",
	})
	if errors.Is(err, agent.ErrUnknownLanguage) {
		return Result{Status: StatusOK, Detail: "refused as expected: unknown language, nothing sent"}
	}
	return classify(nil, err)
}

// --- disruptive checks ---

func checkRestartDecypharr(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
	result, err := disp.Call(ctx, "restart_decypharr", map[string]any{})
	return classify(result, err)
}

func checkRestartJellyfin(ctx context.Context, disp *agent.Dispatcher, _ *Fixtures, _ Options) Result {
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

// checkDecypharrDeleteReadd exercises the delete-and-re-add preview. Like
// checkArrRemoveAndSearch this calls the tool's Handler, which is
// PlanTorrentReadd — a read-only resolve that deletes nothing, so it is safe
// at every tier.
//
// A torrent with no stored magnet is a legitimate refusal, not a failure:
// that refusal is the guard that stops an unrecoverable delete, so surface it
// as degraded rather than fail.
func checkDecypharrDeleteReadd(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.ReaddTorrentName == "" {
		return degraded("no fixture: no torrent with a stored magnet and a unique name discovered")
	}
	result, err := disp.Call(ctx, "decypharr_delete_readd", map[string]any{argName: fx.ReaddTorrentName})
	if err != nil && errors.Is(err, client.ErrNoMagnet) {
		return degraded("fixture torrent has no stored magnet, so a re-add would be " +
			"unrecoverable — the plan correctly refuses")
	}
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
			argReason:    client.ReplaceReasonOther,
		}, "", true
	case fx.MovieTitle != "":
		return map[string]any{
			argMediaType: "movie",
			argTitle:     fx.MovieTitle,
			argReason:    client.ReplaceReasonOther,
		}, "", true
	default:
		return nil, "no series or movie title discovered", false
	}
}

// --- shared helpers ---

const (
	repairIdlePoll = 2 * time.Second
	repairIdleWait = 2 * time.Minute
)

// skipIfRepairRunning consults get_repair_status before a decypharr repair
// action, so the live-check suite never stacks a second repair sweep on top
// of one already in progress (the same rule the systemPrompt gives the
// agent, enforced here for real since the write tools don't self-check it).
// Waits up to repairIdleWait first: refresh_decypharr_links is itself a repair run.
func skipIfRepairRunning(ctx context.Context, disp *agent.Dispatcher) (bool, Result) {
	deadline := time.Now().Add(repairIdleWait)
	for {
		raw, err := disp.Decypharr.RepairStatus(ctx)
		if err != nil {
			// Can't determine status — proceed rather than block the check
			// entirely; the action call below will surface any real problem.
			return false, Result{}
		}
		running, recognized := decypharrRepairRunning(raw)
		// recognized was previously discarded here, while env.go's equivalent
		// used it. decypharrRepairStatus unmarshals into a one-field struct,
		// which succeeds for any JSON object — so a build that renames or nests
		// active_run decodes cleanly with the field empty and reads as "not
		// running". That stacks a second sweep onto a live one and races
		// decypharr's own lock: exactly the failure the comment below documents
		// as already having happened once, re-entered through a different door.
		if !recognized {
			return true, Result{
				Status: StatusDegraded,
				Detail: "could not tell whether a decypharr repair is running (unrecognized " +
					"/api/repair/status shape); skipping rather than risk stacking a second sweep",
			}
		}
		if !running {
			return false, Result{}
		}
		if time.Now().After(deadline) {
			detail := fmt.Sprintf("a decypharr repair is still running after waiting %s", repairIdleWait) +
				decypharrActiveRunStageSuffix(raw)
			return true, Result{Status: StatusSkipped, Detail: detail}
		}
		select {
		case <-ctx.Done():
			return true, Result{
				Status: StatusFail,
				Error:  "cancelled while waiting for a decypharr repair to finish",
			}
		case <-time.After(repairIdlePoll):
		}
	}
}

// decypharrActiveRunStageSuffix best-effort appends the active run's stage
// (e.g. "probing") to a skip message. Most sweeps right now are decypharr's
// own due=0 fast path (nothing due yet — recheck_interval is measured in
// days) that completes in under a second, which technically satisfies
// decypharrRepairRunning for that brief window but isn't remotely the same
// as a real multi-minute sweep over hundreds of candidates; the stage at
// least hints at which one this is. Falls back to no suffix if the shape
// can't be parsed — a plainer message beats a wrong one. (Field to add once
// confirmed: a candidate count, if/when its real JSON key is verified live —
// see ROADMAP.)
func decypharrActiveRunStageSuffix(raw json.RawMessage) string {
	var status struct {
		ActiveRun *struct {
			Stage string `json:"stage"`
		} `json:"active_run"`
	}
	if err := json.Unmarshal(raw, &status); err != nil || status.ActiveRun == nil || status.ActiveRun.Stage == "" {
		return ""
	}
	return fmt.Sprintf(" (stage: %s)", status.ActiveRun.Stage)
}

// decypharrRepairStatus is decypharr's GET /api/repair/status response.
// Confirmed against decypharr's own source (pkg/manager/repair.go): Status()
// builds last_run by explicitly skipping the currently-active run (and any
// run still in a running state), so last_run.status can NEVER be "running"
// — a first attempt at this check parsed last_run.status and the guard never
// tripped as a result, letting two sweeps race into decypharr's own lock.
// active_run is the only field populated while a sweep is actually in
// progress, and is set synchronously (the run record is persisted before
// RunNow even returns the run_id to the caller), so there's no
// persistence-lag race to worry about either.
type decypharrRepairStatus struct {
	ActiveRun json.RawMessage `json:"active_run,omitempty"`
}

// decypharrRepairRunning reports whether raw (a GET /api/repair/status
// response) shows a repair currently in progress — active_run present and
// non-null — and whether the shape was recognized at all (false on decode
// failure, distinguishing "definitely idle" from "couldn't tell").
func decypharrRepairRunning(raw json.RawMessage) (bool, bool) {
	var status decypharrRepairStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return false, false
	}
	running := len(status.ActiveRun) > 0 && string(status.ActiveRun) != "null"
	return running, true
}

func degraded(detail string) Result { return Result{Status: StatusDegraded, Detail: detail} }

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

func checkListAlternatives(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.SeriesTitle == "" {
		return degraded("no fixture: no series title discovered")
	}
	result, err := disp.Call(ctx, "list_alternatives",
		map[string]any{argTitle: fx.SeriesTitle, "season": float64(1), "episode": float64(1)})
	return classify(result, err)
}

func checkSonarrSwapPlan(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures, _ Options) Result {
	if fx.SeriesTitle == "" {
		return degraded("no fixture: no series title discovered")
	}
	result, err := disp.Call(ctx, "sonarr_swap_plan", map[string]any{
		argTitle: fx.SeriesTitle, "season": float64(1), "episode": float64(1),
		"release": "livecheck-not-a-release", argLanguage: "en",
	})
	if err != nil && strings.Contains(err.Error(), "not cached") {
		return Result{Status: StatusOK, Detail: err.Error()}
	}
	return classify(result, err)
}
