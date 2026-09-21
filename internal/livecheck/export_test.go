package livecheck

import (
	"context"
	"encoding/json"

	"github.com/minz1/mediafixer/internal/agent"
)

// CheckRegistryForTest exposes checkRegistry for internal/livecheck/coverage_test.go
// (package livecheck_test).
func CheckRegistryForTest() []checkSpec { return checkRegistry() }

// Exported for internal/livecheck/*_test.go (package livecheck_test).

func DecypharrRepairRunningForTest(raw json.RawMessage) (bool, bool) {
	return decypharrRepairRunning(raw)
}

func FirstRepairEntryNameForTest(raw json.RawMessage) (string, bool) {
	return firstRepairEntryName(raw)
}

func DecypharrActiveRunStageSuffixForTest(raw json.RawMessage) string {
	return decypharrActiveRunStageSuffix(raw)
}

func DecypharrCandidateDirsForTest(folder, name string) []string {
	return decypharrCandidateDirs(folder, name)
}

// DiscoverJellyfinItemForTest exposes the Jellyfin fixture discovery path so
// fixtures_test.go can pin which item it settles on.
func DiscoverJellyfinItemForTest(ctx context.Context, disp *agent.Dispatcher, fx *Fixtures) {
	discoverJellyfinItem(ctx, disp, fx)
}
