package livecheck

import (
	"context"
	"fmt"

	"github.com/minz1/mediafixer/internal/agent"
	"github.com/minz1/mediafixer/internal/mediaagentapi"
)

// runEnvChecks reports on the health of the media stack itself — mount
// state, in-progress jobs — separately from tool health, so a dead FUSE
// mount (an environment problem) doesn't read as a broken tool.
func runEnvChecks(ctx context.Context, disp *agent.Dispatcher) []Result {
	var results []Result
	if r, ok := checkMountHealth(ctx, disp); ok {
		results = append(results, r)
	}
	results = append(results, checkJellyfinScanEnv(ctx, disp))
	results = append(results, checkDecypharrRepairEnv(ctx, disp))
	return results
}

const (
	envMountPath           = "/mnt/decypharr"
	envToolJellyfinScan    = "jellyfin scan"
	envToolDecypharrRepair = "decypharr repair"
)

func checkMountHealth(ctx context.Context, disp *agent.Dispatcher) (Result, bool) {
	if disp.MediaAgent == nil {
		return Result{}, false
	}
	disk, err := disp.MediaAgent.DiskUsage(ctx)
	if err != nil {
		return Result{Tool: envMountPath, Status: StatusFail, Error: err.Error()}, true
	}
	for _, m := range disk.Mounts {
		if m.Path != envMountPath {
			continue
		}
		return mountResult(m), true
	}
	return Result{
		Tool:   envMountPath,
		Status: StatusDegraded,
		Detail: "not present in media-agent's configured mounts",
	}, true
}

// mountResult applies the exact accessible/is_mount_point interpretation
// documented on the get_disk_info tool: is_mount_point=false means the FUSE
// mount died and fell back to an empty directory, even though it may still
// look "accessible".
func mountResult(m mediaagentapi.DiskMount) Result {
	switch {
	case !m.Accessible:
		return Result{
			Tool:   envMountPath,
			Status: StatusFail,
			Detail: "not accessible — mount entry stale, FUSE daemon likely dead",
		}
	case !m.IsMountPoint:
		return Result{
			Tool:   envMountPath,
			Status: StatusFail,
			Detail: "accessible but NOT a mount point — FUSE mount died and fell back to an empty directory",
		}
	default:
		return Result{Tool: envMountPath, Status: StatusOK, Detail: "mounted"}
	}
}

func checkJellyfinScanEnv(ctx context.Context, disp *agent.Dispatcher) Result {
	status, err := disp.Jellyfin.ScanStatus(ctx)
	if err != nil {
		return Result{Tool: envToolJellyfinScan, Status: StatusFail, Error: err.Error()}
	}
	if status.Running {
		return Result{
			Tool:   envToolJellyfinScan,
			Status: StatusOK,
			Detail: fmt.Sprintf("running (%.0f%%)", status.ProgressPct),
		}
	}
	return Result{Tool: envToolJellyfinScan, Status: StatusOK, Detail: "idle"}
}

func checkDecypharrRepairEnv(ctx context.Context, disp *agent.Dispatcher) Result {
	raw, err := disp.Decypharr.RepairStatus(ctx)
	if err != nil {
		return Result{Tool: envToolDecypharrRepair, Status: StatusFail, Error: err.Error()}
	}
	running, recognized := decypharrRepairRunning(raw)
	if !recognized {
		return Result{Tool: envToolDecypharrRepair, Status: StatusDegraded, Detail: "unrecognized repair status shape"}
	}
	if running {
		return Result{Tool: envToolDecypharrRepair, Status: StatusOK, Detail: "running"}
	}
	return Result{Tool: envToolDecypharrRepair, Status: StatusOK, Detail: "idle"}
}
