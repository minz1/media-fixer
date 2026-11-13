package operation_test

import (
	"slices"
	"testing"

	"github.com/minz1/mediafixer/internal/operation"
)

func TestTorrentReaddRetired_NoKindOrSteps(t *testing.T) {
	t.Parallel()
	if want := []operation.Kind{operation.KindReplace, operation.KindRegrab}; !slices.Equal(operation.Kinds(), want) {
		t.Errorf("Kinds() = %v, want %v", operation.Kinds(), want)
	}
	for _, k := range operation.StepKinds() {
		if k == "torrent_delete" || k == "torrent_add" {
			t.Errorf("StepKinds() still has %q", k)
		}
	}
}
