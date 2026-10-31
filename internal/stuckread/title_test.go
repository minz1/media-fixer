package stuckread_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/stuckread"
)

func TestTitle(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/mnt/decypharr/__all__/Show.S01.1080p/e1.mkv": "Show.S01.1080p",
		"/data/downloads/sonarr/Show.S01/e1.mkv":       "Show.S01",
		"/data/media/tv/Show/S01/e1.mkv":               "e1.mkv",
		"/mnt/decypharr/__all__/e1.mkv":                "e1.mkv",
	}
	for path, want := range cases {
		if got := stuckread.Title(path); got != want {
			t.Errorf("Title(%q) = %q, want %q", path, got, want)
		}
	}
}
