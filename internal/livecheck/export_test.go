package livecheck

import "encoding/json"

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

func DecypharrCandidateDirsForTest(torrentName string) []string {
	return decypharrCandidateDirs(torrentName)
}
