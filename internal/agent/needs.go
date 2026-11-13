package agent

import "errors"

var (
	errSonarrNotConfigured = errors.New("sonarr not configured")
	errRadarrNotConfigured = errors.New("radarr not configured")
)

type toolNeed func(*Dispatcher) (string, bool)

func needDecypharr(d *Dispatcher) (string, bool) {
	if d.Decypharr.Configured() {
		return "", true
	}
	return "decypharr not configured", false
}

func needJellyfin(d *Dispatcher) (string, bool) {
	if d.Jellyfin.Configured() {
		return "", true
	}
	return "jellyfin not configured", false
}

func needSonarr(d *Dispatcher) (string, bool) {
	if d.Sonarr.Configured() {
		return "", true
	}
	return errSonarrNotConfigured.Error(), false
}

func needRadarr(d *Dispatcher) (string, bool) {
	if d.Radarr.Configured() {
		return "", true
	}
	return errRadarrNotConfigured.Error(), false
}

func needArr(d *Dispatcher) (string, bool) {
	if d.Sonarr.Configured() || d.Radarr.Configured() {
		return "", true
	}
	return "sonarr and radarr not configured", false
}

func needBazarr(d *Dispatcher) (string, bool) {
	if d.Bazarr.Configured() {
		return "", true
	}
	return "bazarr not configured", false
}

func needBazarrAndArr(d *Dispatcher) (string, bool) {
	if detail, ok := needBazarr(d); !ok {
		return detail, false
	}
	return needArr(d)
}

func needMediaAgent(d *Dispatcher) (string, bool) {
	if d.MediaAgent.Configured() {
		return "", true
	}
	return errMediaAgentNotConfigured.Error(), false
}

func needArrAndDecypharr(d *Dispatcher) (string, bool) {
	if detail, ok := needArr(d); !ok {
		return detail, false
	}
	return needDecypharr(d)
}

func needJellyfinAndMediaAgent(d *Dispatcher) (string, bool) {
	if detail, ok := needJellyfin(d); !ok {
		return detail, false
	}
	return needMediaAgent(d)
}

func needLoki(d *Dispatcher) (string, bool) {
	if err := d.Loki.Err(); err != nil {
		return err.Error(), false
	}
	if d.Loki.Configured() {
		return "", true
	}
	return errLokiNotConfigured.Error(), false
}

func toolNeeds() map[string]toolNeed {
	return map[string]toolNeed{
		toolJellyfinSearch:       needJellyfin,
		toolJellyfinPlayback:     needJellyfin,
		toolJellyfinListEpisodes: needJellyfin,
		toolJellyfinScanStatus:   needJellyfin,
		toolJellyfinLibraryScan:  needJellyfin,
		toolClearJellyfinCache:   needJellyfin,
		toolBazarrSearch:         needBazarrAndArr,
		toolGetTorrentState:      needDecypharr,
		toolRepairStatus:         needDecypharr,
		toolRepairHealth:         needDecypharr,
		toolRefreshLinks:         needDecypharr,
		toolRepairSweep:          needDecypharr,
		toolCacheCleanup:         needDecypharr,
		toolDecypharrRecheck:     needDecypharr,
		toolDecypharrDeleteReadd: needDecypharr,
		toolArrMediaStatus:       needArr,
		toolArrGrabHistory:       needArr,
		toolArrSearchMissing:     needArr,
		toolArrRemoveAndSearch:   needArr,
		toolSonarrRescan:         needSonarr,
		toolRadarrRescan:         needRadarr,
		toolDDReadability:        needMediaAgent,
		toolProbeStreams:         needMediaAgent,
		toolStreamAlignment:      needJellyfinAndMediaAgent,
		toolSubtitleExtractTest:  needJellyfin,
		toolListAlternatives:     needArrAndDecypharr,
		toolSonarrSwapPlan:       needSwapPlan,
		toolListDirectory:        needMediaAgent,
		toolGetDiskInfo:          needMediaAgent,
		toolRestartDecypharr:     needMediaAgent,
		toolRestartJellyfin:      needMediaAgent,
		toolLokiQuery:            needLoki,
	}
}

type NeedState int

const (
	NeedMet NeedState = iota
	NeedUnconfigured
	NeedBroken
)

func NeedOf(d *Dispatcher, tool string) (string, NeedState) {
	spec, ok := specByName(tool)
	if !ok || spec.Needs == nil {
		return "", NeedMet
	}
	reason, met := spec.Needs(d)
	switch {
	case met:
		return "", NeedMet
	case tool == toolLokiQuery && d.Loki.Err() != nil:
		return reason, NeedBroken
	}
	return reason, NeedUnconfigured
}

func needSwapPlan(d *Dispatcher) (string, bool) {
	if detail, ok := needSonarr(d); !ok {
		return detail, false
	}
	return needJellyfinAndMediaAgent(d)
}
