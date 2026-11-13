package livecheck_test

import (
	"os"
	"testing"

	"github.com/minz1/mediafixer/internal/livecheck"
)

func TestDecypharrContract_LivecheckReadsTheRecordedShapes(t *testing.T) {
	t.Parallel()
	status, err := os.ReadFile("../client/testdata/decypharr/repair_status_running.json")
	if err != nil {
		t.Fatal(err)
	}
	running, recognized := livecheck.DecypharrRepairRunningForTest(status)
	if !running || !recognized {
		t.Errorf("repair status: running=%v recognized=%v, want both true", running, recognized)
	}
	if got := livecheck.DecypharrActiveRunStageSuffixForTest(status); got != " (stage: probing)" {
		t.Errorf("stage suffix %q", got)
	}

	health, err := os.ReadFile("../client/testdata/decypharr/repair_health.json")
	if err != nil {
		t.Fatal(err)
	}
	name, ok := livecheck.FirstRepairEntryNameForTest(health)
	if !ok || name != "Show.Name.S01E06.1080p.WEB.h264-GROUP" {
		t.Errorf("health entry name %q ok=%v", name, ok)
	}
}
