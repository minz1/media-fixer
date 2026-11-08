package sweepstate_test

import (
	"testing"
	"time"

	"github.com/minz1/mediafixer/internal/sweep/sweepstate"
)

const day = 24 * time.Hour

func blocked(c sweepstate.RefusalCode, age time.Duration) bool {
	return c.Blocks() && age <= c.Memory()
}

func TestS5_1_OtherRefusalBlocksSevenDays(t *testing.T) {
	t.Parallel()
	c := sweepstate.OtherRefusal
	if c.Memory() != 7*day || !c.Blocks() {
		t.Fatalf("other: memory %v, blocks %v", c.Memory(), c.Blocks())
	}
	if !blocked(c, 7*day) || blocked(c, 7*day+time.Nanosecond) || !blocked(c, day) {
		t.Error("other refusal window is not seven days")
	}
}

func TestS5_2_TorrentBlockedNinetyDays(t *testing.T) {
	t.Parallel()
	c := sweepstate.TorrentBlocked
	if c.Memory() != 90*day || !c.Blocks() {
		t.Fatalf("torrent_blocked: memory %v, blocks %v", c.Memory(), c.Blocks())
	}
	if !blocked(c, 89*day) || blocked(c, 91*day) {
		t.Error("torrent_blocked window is not ninety days")
	}
	if sweepstate.MaxRefusalMemory() != 90*day {
		t.Errorf("max memory = %v", sweepstate.MaxRefusalMemory())
	}
}

func TestS5_3_ProviderErrorNeverBlocksAcrossRuns(t *testing.T) {
	t.Parallel()
	c := sweepstate.ProviderError
	if c.Memory() != 0 || c.Blocks() {
		t.Fatalf("provider_error: memory %v, blocks %v", c.Memory(), c.Blocks())
	}
	if blocked(c, 0) || blocked(c, time.Hour) {
		t.Error("provider_error blocked")
	}
}

func TestRefusalCodes_StoredValues(t *testing.T) {
	t.Parallel()
	want := []string{"torrent_blocked", "provider_error", "other"}
	got := sweepstate.RefusalCodes()
	if len(got) != len(want) {
		t.Fatalf("codes = %v", got)
	}
	for i, c := range got {
		if string(c) != want[i] {
			t.Errorf("code %d = %q, want %q", i, c, want[i])
		}
	}
}
